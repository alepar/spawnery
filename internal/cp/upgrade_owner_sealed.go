package cp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/secrets/journalkey"
	"spawnery/internal/secrets/seal"
)

// defaultUpgradeTimeout is how long UpgradeToOwnerSealed waits for the node's
// SealJournalKeyToOwnerResponse. The node has to read its local key storage and seal each
// journaled mount, which is a pure-crypto operation with no network; 10s is generous.
const defaultUpgradeTimeout = 10 * time.Second

// upgradeWaiters correlates an UpgradeToOwnerSealed request with the async
// SealJournalKeyToOwnerResponse the node sends back on its Attach stream.
// Keyed by a per-request request_id (not spawn_id) to prevent stale acks from leaking
// into subsequent requests — same discipline as modelWaiters.
type upgradeWaiters struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.SealJournalKeyToOwnerResponse
}

func newUpgradeWaiters() *upgradeWaiters {
	return &upgradeWaiters{m: map[string]chan *nodev1.SealJournalKeyToOwnerResponse{}}
}

func (w *upgradeWaiters) register(requestID string) chan *nodev1.SealJournalKeyToOwnerResponse {
	ch := make(chan *nodev1.SealJournalKeyToOwnerResponse, 1)
	w.mu.Lock()
	w.m[requestID] = ch
	w.mu.Unlock()
	return ch
}

func (w *upgradeWaiters) unregister(requestID string) {
	w.mu.Lock()
	delete(w.m, requestID)
	w.mu.Unlock()
}

// deliver routes an inbound SealJournalKeyToOwnerResponse to its waiter (if any).
func (w *upgradeWaiters) deliver(r *nodev1.SealJournalKeyToOwnerResponse) {
	w.mu.Lock()
	ch, ok := w.m[r.GetRequestId()]
	w.mu.Unlock()
	if !ok {
		return // stale / duplicate — drop
	}
	select {
	case ch <- r:
	default:
	}
}

// UpgradeToOwnerSealed instructs the spawn's hosting node to seal each journaled mount's
// repo password to the owner's device set (the cheap node-local → owner-sealed upgrade —
// no re-encryption). On success the resulting per-mount Envelopes are stored via
// PutJournalKeyCiphertext, making subsequent cross-node MigrateSpawn moves valid.
//
// This is the CP side of the upgrade seam (sp-8dkp §4). The node-side handler lives in
// internal/spawnlet/sealtoowner.go; it reads the node-local custody and calls
// journalkey.SealToOwner.
func (s *Server) UpgradeToOwnerSealed(ctx context.Context, req *connect.Request[cpv1.UpgradeToOwnerSealedRequest]) (*connect.Response[cpv1.UpgradeToOwnerSealedResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	if len(req.Msg.OwnerDevicePubkeys) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("owner_device_pubkeys is required"))
	}
	// Validate all pubkeys are 32 bytes (raw X25519).
	for i, pk := range req.Msg.OwnerDevicePubkeys {
		if len(pk) != 32 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("owner_device_pubkeys[%d]: want 32 bytes, got %d", i, len(pk)))
		}
	}

	// Look up the spawn's live container.
	c, ok, err := s.st.Spawns().LiveContainer(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("spawn is not running (no live container)"))
	}

	// Look up the hosting node's sender.
	node, nodeOK := s.reg.Get(c.NodeID)
	if !nodeOK {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("hosting node %q is not connected", c.NodeID))
	}

	// Mint a request id and register the waiter BEFORE sending so a fast response isn't missed.
	reqID := uuid.NewString()
	ch := s.upgradeWaiters.register(reqID)
	defer s.upgradeWaiters.unregister(reqID)

	// Send the SealJournalKeyToOwnerRequest down the node Attach stream.
	if err := node.Sender.Send(&nodev1.CPMessage{
		Msg: &nodev1.CPMessage_SealJournalKey{
			SealJournalKey: &nodev1.SealJournalKeyToOwnerRequest{
				SpawnId:             req.Msg.SpawnId,
				Generation:          uint64(c.Generation),
				OwnerDevicePubkeys:  req.Msg.OwnerDevicePubkeys,
				RequestId:           reqID,
				OwnerId:             owner,
			},
		},
	}); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("send to node: %w", err))
	}

	// Await the response.
	timeout := s.upgradeTimeout
	if timeout == 0 {
		timeout = defaultUpgradeTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("node seal error: %s", resp.Error))
		}
		// Store each sealed envelope via the same custody path PutJournalKeyCiphertext uses.
		for _, e := range resp.Entries {
			if e.Mount == "" || len(e.Ciphertext) == 0 {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("node returned empty entry"))
			}
			// Validate the envelope is openable by the owner's devices (fail-closed guard).
			devices := make([]seal.X25519PubKey, len(req.Msg.OwnerDevicePubkeys))
			for i, pk := range req.Msg.OwnerDevicePubkeys {
				devices[i] = seal.X25519PubKey(pk)
			}
			if err := assertOpenableByOwner(e.Ciphertext, devices); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mount %q: sealed ciphertext not openable by owner: %w", e.Mount, err))
			}
			if perr := s.journalKeys.Put(ctx, req.Msg.SpawnId, journalkey.SecretID(e.Mount), e.Ciphertext); perr != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store journal key ciphertext for mount %q: %w", e.Mount, perr))
			}
		}
		_ = owner
		return connect.NewResponse(&cpv1.UpgradeToOwnerSealedResponse{}), nil

	case <-timer.C:
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("timed out waiting for node to seal journal key (timeout: %v)", timeout))

	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, fmt.Errorf("context canceled"))
	}
}
