package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/cp/registry"
)

func TestAuthorizeGitHubMintRequiresIndexedHostedNode(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	err := s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-1", SpawnID: "sp1", Generation: 1, SecretID: "gh-main",
	})
	if err != nil {
		t.Fatalf("authorize hosted indexed node: %v", err)
	}
	err = s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-2", SpawnID: "sp1", Generation: 1, SecretID: "gh-main",
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorize wrong node code=%v err=%v", connect.CodeOf(err), err)
	}
	err = s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-1", SpawnID: "sp1", Generation: 1, SecretID: "not-indexed",
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorize unindexed link code=%v err=%v", connect.CodeOf(err), err)
	}
}

func TestAuthorizeGitHubMintRPCConfirmsIndexedHostedNode(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	_, err := s.AuthorizeGitHubMint(context.Background(), connect.NewRequest(&cpv1.AuthorizeGitHubMintRequest{
		NodeId: "node-1", SpawnId: "sp1", Generation: 1, SecretId: "gh-main", Version: 12, DeliveryId: "d1",
	}))
	if err != nil {
		t.Fatalf("AuthorizeGitHubMint RPC: %v", err)
	}
}

// githubTokenRotateds returns all GitHubTokenRotatedSignal messages delivered by this sender.
func (c *capSender) githubTokenRotateds() []*nodev1.GitHubTokenRotatedSignal {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*nodev1.GitHubTokenRotatedSignal
	for _, m := range c.sent {
		if sig := m.GetGithubTokenRotated(); sig != nil {
			out = append(out, sig)
		}
	}
	return out
}

func TestSignalGitHubTokenRotatedRelaysToIndexedLiveSpawns(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender1 := &capSender{}
	sender2 := &capSender{}
	reg.Add(&registry.Node{ID: "node-1", Sender: sender1})
	reg.Add(&registry.Node{ID: "node-2", Sender: sender2})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	createActiveSpawn(t, s, "alice", "sp2", "node-2")
	s.githubLinks.note("sp1", "gh-main")
	s.githubLinks.note("sp2", "gh-main")

	// node-3: disconnected — should be skipped without aborting the call.
	createActiveSpawn(t, s, "alice", "sp3", "node-3")
	s.githubLinks.note("sp3", "gh-main")

	err := s.signalGitHubTokenRotated(context.Background(), "gh-main", 12, "github-access-gh-main-v12", 1893420000)
	if err != nil {
		t.Fatalf("signalGitHubTokenRotated: %v", err)
	}

	sigs1 := sender1.githubTokenRotateds()
	sigs2 := sender2.githubTokenRotateds()
	if len(sigs1) != 1 {
		t.Fatalf("sender1 signals = %d, want 1: %+v", len(sigs1), sigs1)
	}
	if len(sigs2) != 1 {
		t.Fatalf("sender2 signals = %d, want 1: %+v", len(sigs2), sigs2)
	}
	sig1 := sigs1[0]
	if sig1.GetSpawnId() != "sp1" || sig1.GetSecretId() != "gh-main" ||
		sig1.GetVersion() != 12 || sig1.GetDeliveryId() != "github-access-gh-main-v12" ||
		sig1.GetAccessExpiresAtUnix() != 1893420000 {
		t.Fatalf("sig1 = %+v", sig1)
	}
	// Generation must be stamped from the live generation (1 for sp1).
	if sig1.GetGeneration() != 1 {
		t.Fatalf("sig1 generation = %d, want 1 (live)", sig1.GetGeneration())
	}
	sig2 := sigs2[0]
	if sig2.GetSpawnId() != "sp2" || sig2.GetGeneration() != 1 {
		t.Fatalf("sig2 = %+v", sig2)
	}
}

func TestSignalGitHubTokenRotatedRPCRelaysSignal(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "node-1", Sender: sender})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	_, err := s.SignalGitHubTokenRotated(context.Background(), connect.NewRequest(&cpv1.SignalGitHubTokenRotatedRequest{
		SecretId: "gh-main", Version: 12, DeliveryId: "github-access-gh-main-v12", AccessExpiresAtUnix: 1893420000,
	}))
	if err != nil {
		t.Fatalf("SignalGitHubTokenRotated RPC: %v", err)
	}
	sigs := sender.githubTokenRotateds()
	if len(sigs) != 1 || sigs[0].GetSpawnId() != "sp1" || sigs[0].GetSecretId() != "gh-main" || sigs[0].GetVersion() != 12 {
		t.Fatalf("signals = %+v", sigs)
	}
}
