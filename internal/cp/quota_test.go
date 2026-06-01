package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

func seedActiveSpawn(t *testing.T, s *Server, owner, id string) {
	t.Helper()
	if err := s.st.Spawns().Create(context.Background(), store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Active, CreatedAt: 1, LastUsedAt: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPerUserSpawnCap(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetMaxSpawnsPerOwner(1)
	ctx := auth.WithOwner(context.Background(), "alice")
	seedActiveSpawn(t, s, "alice", "existing")
	_, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
}

func TestPerUserSpawnQuotaHelper(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	seedActiveSpawn(t, s, "alice", "existing")
	// default cap 0 = unlimited -> no rejection
	if err := s.checkSpawnQuota(ctx, "alice"); err != nil {
		t.Fatalf("unlimited cap must not reject: %v", err)
	}
	// cap 1 with one existing -> rejected
	s.SetMaxSpawnsPerOwner(1)
	if err := s.checkSpawnQuota(ctx, "alice"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	// a different owner with no spawns -> ok
	if err := s.checkSpawnQuota(ctx, "bob"); err != nil {
		t.Fatalf("bob (no spawns) must not be capped: %v", err)
	}
}
