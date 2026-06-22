package cp

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/cp/store"
)

type githubLinkIndex struct {
	mu       sync.Mutex
	bySecret map[string]map[string]struct{}
}

func newGitHubLinkIndex() *githubLinkIndex {
	return &githubLinkIndex{bySecret: map[string]map[string]struct{}{}}
}

func (i *githubLinkIndex) noteNodeSecrets(spawnID string, secrets []*nodev1.SealedSecret) {
	for _, sec := range secrets {
		if sec.GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN || sec.GetSecretId() == "" {
			continue
		}
		i.note(spawnID, sec.GetSecretId())
	}
}

func (i *githubLinkIndex) noteCPSecrets(spawnID string, secrets []*cpv1.SealedSecret) {
	for _, sec := range secrets {
		if sec.GetType() != cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN || sec.GetSecretId() == "" {
			continue
		}
		i.note(spawnID, sec.GetSecretId())
	}
}

func (i *githubLinkIndex) note(spawnID, secretID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	spawns := i.bySecret[secretID]
	if spawns == nil {
		spawns = map[string]struct{}{}
		i.bySecret[secretID] = spawns
	}
	spawns[spawnID] = struct{}{}
}

func (i *githubLinkIndex) has(secretID, spawnID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.bySecret[secretID][spawnID]
	return ok
}

func (i *githubLinkIndex) spawns(secretID string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	spawns := i.bySecret[secretID]
	out := make([]string, 0, len(spawns))
	for spawnID := range spawns {
		out = append(out, spawnID)
	}
	return out
}

func (s *Server) authorizeGitHubMint(ctx context.Context, req authsvc.GitHubMintAuthorization) error {
	nodeID, generation, err := s.liveNode(ctx, req.SpawnID)
	if err != nil {
		return err
	}
	if nodeID != req.NodeID || generation != req.Generation {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("node does not host spawn generation"))
	}
	if !s.githubLinks.has(req.SecretID, req.SpawnID) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("spawn is not indexed for github link"))
	}
	return nil
}

func (s *Server) signalGitHubTokenRotated(ctx context.Context, secretID string, version uint64, deliveryID string, accessExpiresAtUnix int64) error {
	if secretID == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("secret_id is required"))
	}
	for _, spawnID := range s.githubLinks.spawns(secretID) {
		nodeID, generation, err := s.liveNode(ctx, spawnID)
		if err != nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				continue
			}
			return err
		}
		// Best-effort fanout: per-spawn problems (disconnected node, transient send failure) must
		// not deny the requesting node its token. Such spawns re-sync on reconnect/resume.
		n, ok := s.reg.Get(nodeID)
		if !ok || n.Sender == nil {
			log.Printf("signalGitHubTokenRotated %s: skipping disconnected hosting node %q for spawn %q (will re-sync on reconnect)", secretID, nodeID, spawnID)
			continue
		}
		if err := n.Sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_GithubTokenRotated{GithubTokenRotated: &nodev1.GitHubTokenRotatedSignal{
			SpawnId:             spawnID,
			Generation:          generation,
			SecretId:            secretID,
			Version:             version,
			DeliveryId:          deliveryID,
			AccessExpiresAtUnix: accessExpiresAtUnix,
		}}}); err != nil {
			log.Printf("signalGitHubTokenRotated %s: send to hosting node %q for spawn %q failed: %v; will re-sync on reconnect", secretID, nodeID, spawnID, err)
			continue
		}
	}
	return nil
}

// spawnHasGitHubMintMount reports whether any mount is a github mount carrying a CP-derived gh:
// mint link-ref (Approach 2 — minted at provision by the hosting node).
func spawnHasGitHubMintMount(mounts []store.Mount) bool {
	for _, m := range mounts {
		if strings.HasPrefix(m.BackendURI, "github:") && isGitHubMintLinkRef(m.CredentialSecretID) {
			return true
		}
	}
	return false
}

// seedGitHubMintLinks pre-seeds the in-memory github-link index for every gh: mint link-ref mount
// so authorizeGitHubMint admits the hosting node's at-provision JIT mint, which races INSIDE the
// blocking Provision/StartSpawn window (the node mints before acking ACTIVE). The entry needs no
// SealedSecret template — has(secretID, spawnID) is all authorizeGitHubMint consults. A token
// never transits the CP.
func (s *Server) seedGitHubMintLinks(spawnID string, mounts []store.Mount) {
	for _, m := range mounts {
		if strings.HasPrefix(m.BackendURI, "github:") && isGitHubMintLinkRef(m.CredentialSecretID) {
			s.githubLinks.note(spawnID, strings.TrimSpace(m.CredentialSecretID))
		}
	}
}

// prepareGitHubMintProvision makes a github-mint spawn mintable at provision: it seeds the link
// index AND pre-binds the live container to the already-picked target node (Adopt), both BEFORE
// StartSpawn is sent. The pre-bind is required because authorizeGitHubMint also calls liveNode,
// but the gen-N container's node_id is "" until SetActive (which runs AFTER the blocking Provision).
// No-op when the spawn has no gh: mint mount. Called only from the intentEnabled provision branches
// (the dev-github lane — the only lane with a node->AS mint channel and a pre-Provision target node).
func (s *Server) prepareGitHubMintProvision(ctx context.Context, spawnID string, gen uint64, targetNodeID string, mounts []store.Mount) error {
	if !spawnHasGitHubMintMount(mounts) {
		return nil
	}
	s.seedGitHubMintLinks(spawnID, mounts)
	if err := s.st.Spawns().Adopt(ctx, spawnID, targetNodeID, int64(gen)); err != nil {
		return fmt.Errorf("pre-bind node %q for github mint: %w", targetNodeID, err)
	}
	return nil
}

func (s *Server) AuthorizeGitHubMint(ctx context.Context, req *connect.Request[cpv1.AuthorizeGitHubMintRequest]) (*connect.Response[cpv1.AuthorizeGitHubMintResponse], error) {
	msg := req.Msg
	if msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request is required"))
	}
	if err := s.authorizeGitHubMint(ctx, authsvc.GitHubMintAuthorization{
		NodeID:       msg.GetNodeId(),
		SpawnID:      msg.GetSpawnId(),
		Generation:   msg.GetGeneration(),
		SecretID:     msg.GetSecretId(),
		Version:      msg.GetVersion(),
		DeliveryID:   msg.GetDeliveryId(),
		RepositoryID: msg.GetRepositoryId(),
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.AuthorizeGitHubMintResponse{}), nil
}

func (s *Server) SignalGitHubTokenRotated(ctx context.Context, req *connect.Request[cpv1.SignalGitHubTokenRotatedRequest]) (*connect.Response[cpv1.SignalGitHubTokenRotatedResponse], error) {
	msg := req.Msg
	if msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("request is required"))
	}
	if err := s.signalGitHubTokenRotated(ctx, msg.GetSecretId(), msg.GetVersion(), msg.GetDeliveryId(), msg.GetAccessExpiresAtUnix()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.SignalGitHubTokenRotatedResponse{}), nil
}
