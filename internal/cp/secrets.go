package cp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
	"spawnery/internal/secrets/journalkey"
)

// Owner-sealed secret delivery, CP side (sp-2ckv.4, design §3). The CP is a CIPHERTEXT-ONLY relay: it
// caches each node's published HPKE sub-key + relayed cert chain, hands them to verified owner clients
// (GetSpawnNodeKey), and forwards owner-sealed ciphertext to the hosting node (DeliverSecrets). It never
// holds plaintext and never unseals — a fully compromised CP yields ciphertext + metadata, never secrets.

// nodeKeyCache holds, per node, its last-published SignedSubKey JSON and the leaf+chain PEM relayed from
// its mTLS connection. Refreshed on Register and on a rotation-carrying Heartbeat.
type nodeKeyCache struct {
	mu sync.Mutex
	m  map[string]nodeKeyEntry
}

type nodeKeyEntry struct {
	subkey    []byte // JSON subkey.SignedSubKey (opaque to the CP)
	certChain []byte // node leaf+chain PEM (empty in insecure mode)
}

func newNodeKeyCache() *nodeKeyCache { return &nodeKeyCache{m: map[string]nodeKeyEntry{}} }

// put records nodeID's sub-key + cert chain. A delivery with no sub-key (an insecure/dev node that
// publishes none) is ignored — it must not wipe a previously cached real sub-key.
func (c *nodeKeyCache) put(nodeID string, subkey, certChain []byte) {
	if len(subkey) == 0 {
		return
	}
	c.mu.Lock()
	c.m[nodeID] = nodeKeyEntry{
		subkey:    append([]byte(nil), subkey...),
		certChain: append([]byte(nil), certChain...),
	}
	c.mu.Unlock()
}

func (c *nodeKeyCache) get(nodeID string) (nodeKeyEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[nodeID]
	return e, ok
}

// liveNode resolves a spawn's live hosting node id + episode generation (the binding GetSpawnNodeKey and
// DeliverSecrets share). Returns FailedPrecondition when the spawn has no live container.
func (s *Server) liveNode(ctx context.Context, spawnID string) (nodeID string, generation uint64, err error) {
	c, hasLive, lerr := s.st.Spawns().LiveContainer(ctx, spawnID)
	if lerr != nil {
		return "", 0, connect.NewError(connect.CodeInternal, lerr)
	}
	if !hasLive || c.NodeID == "" {
		return "", 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn has no live container"))
	}
	return c.NodeID, uint64(c.Generation), nil
}

type secretDeliveryTarget struct {
	nodeID      string
	generation  uint64
	pendingFork bool
}

type forkTargetKeyMaterial struct {
	signedSubKey []byte
	certChain    []byte
}

func (s *Server) forkTargetKeyMaterial(nodeID string) (forkTargetKeyMaterial, error) {
	entry, ok := s.nodeKeys.get(nodeID)
	if !ok || len(entry.subkey) == 0 {
		return forkTargetKeyMaterial{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target node %q has not published an HPKE sub-key", nodeID))
	}
	if len(entry.certChain) == 0 {
		return forkTargetKeyMaterial{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target node %q has not published a cert chain", nodeID))
	}
	return forkTargetKeyMaterial{
		signedSubKey: append([]byte(nil), entry.subkey...),
		certChain:    append([]byte(nil), entry.certChain...),
	}, nil
}

func (s *Server) resolveSecretDeliveryTarget(ctx context.Context, spawnID string) (secretDeliveryTarget, error) {
	if nodeID, generation, err := s.liveNode(ctx, spawnID); err == nil {
		return secretDeliveryTarget{nodeID: nodeID, generation: generation}, nil
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return secretDeliveryTarget{}, err
	}

	ts, err := s.st.TransferSets().GetPendingForkByForkSpawnID(ctx, spawnID)
	if errors.Is(err, store.ErrNotFound) {
		return secretDeliveryTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn has no live container"))
	}
	if err != nil {
		return secretDeliveryTarget{}, connect.NewError(connect.CodeInternal, err)
	}
	if ts.TargetNodeID == "" || ts.TargetGeneration == 0 {
		return secretDeliveryTarget{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("pending fork has no target node/generation"))
	}
	return secretDeliveryTarget{nodeID: ts.TargetNodeID, generation: ts.TargetGeneration, pendingFork: true}, nil
}

// GetSpawnNodeKey returns the hosting node's relayed key material so the owner client can verify the
// node and seal to it (design §3 steps 1–3). Owner-only + ownership-checked (ownSpawn). The CP relays
// the sub-key + cert chain untrusted — the client re-verifies the chain against pinned roots and the
// sub-key signature against the leaf, so a compromised CP cannot mint trust.
func (s *Server) GetSpawnNodeKey(ctx context.Context, req *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	target, err := s.resolveSecretDeliveryTarget(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, err
	}
	entry, ok := s.nodeKeys.get(target.nodeID)
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("hosting node has not published an HPKE sub-key"))
	}
	return connect.NewResponse(&cpv1.GetSpawnNodeKeyResponse{
		NodeCertChain: entry.certChain,
		SignedSubkey:  entry.subkey,
		Generation:    target.generation,
	}), nil
}

func cpSecretsToNode(secrets []*cpv1.SealedSecret) []*nodev1.SealedSecret {
	out := make([]*nodev1.SealedSecret, len(secrets))
	for i, sec := range secrets {
		out[i] = &nodev1.SealedSecret{
			TargetPath: sec.TargetPath,
			Sealed:     append([]byte(nil), sec.Sealed...),
			SecretId:   sec.SecretId,
		}
	}
	return out
}

func (s *Server) deliverSecretsToPendingForkTarget(spawnID string, target secretDeliveryTarget, secrets []*nodev1.SealedSecret) error {
	n, ok := s.reg.Get(target.nodeID)
	if !ok || n.Sender == nil {
		return fmt.Errorf("target node %q is not connected", target.nodeID)
	}
	return n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_SecretDelivery{SecretDelivery: &nodev1.SecretDelivery{
		SpawnId:    spawnID,
		Generation: target.generation,
		Secrets:    secrets,
	}}})
}

// DeliverSecrets relays owner-sealed ciphertext to the spawn's live node (design §3 step 4). Owner-only
// + ownership-checked. The CP stores nothing in plaintext and never unseals: it copies the opaque
// `sealed` bytes straight through and stamps the live generation so the node can fence a stale episode.
func (s *Server) DeliverSecrets(ctx context.Context, req *connect.Request[cpv1.DeliverSecretsRequest]) (*connect.Response[cpv1.DeliverSecretsResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	if len(req.Msg.Secrets) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("no secrets to deliver"))
	}
	target, err := s.resolveSecretDeliveryTarget(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, err
	}
	// Translate cp.v1.SealedSecret -> node.v1.SealedSecret, copying the ciphertext bytes opaquely.
	secrets := cpSecretsToNode(req.Msg.Secrets)
	var derr error
	if target.pendingFork {
		derr = s.deliverSecretsToPendingForkTarget(req.Msg.SpawnId, target, secrets)
	} else {
		derr = s.rt.DeliverSecrets(req.Msg.SpawnId, target.generation, secrets)
	}
	if derr != nil {
		return nil, connect.NewError(connect.CodeUnavailable, derr)
	}
	// Clear delivery-pending when a journal key is included in the delivery (sp-8dkp §5).
	for _, sec := range req.Msg.Secrets {
		if journalkey.IsJournalKey(sec.SecretId) {
			s.deliveryPending.clear(req.Msg.SpawnId)
			break
		}
	}
	return connect.NewResponse(&cpv1.DeliverSecretsResponse{}), nil
}
