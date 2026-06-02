package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// makeSpawn inserts a spawn row (status=starting) directly via the store (no node flow needed).
// It also upserts the owner so FK constraints are satisfied for owners not in the default seed.
func makeSpawn(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: 1}); err != nil {
		t.Fatalf("seed owner %s: %v", owner, err)
	}
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, nil) }); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
}

func TestListSpawns(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	makeSpawn(t, s, "sp2", "alice")
	makeSpawn(t, s, "sp3", "bob")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.Spawns
	if len(got) != 2 {
		t.Fatalf("alice sees %d spawns, want 2", len(got))
	}
	for _, sm := range got {
		if sm.Status != cpv1.SpawnStatus_SPAWN_STATUS_STARTING {
			t.Fatalf("spawn %s status=%v want STARTING", sm.SpawnId, sm.Status)
		}
		if sm.AppId != "secret-app" || sm.AppVersion != "1.0.0" || sm.Model != "m" {
			t.Fatalf("summary fields wrong: %+v", sm)
		}
	}
	if _, err := s.ListSpawns(context.Background(), connect.NewRequest(&cpv1.ListSpawnsRequest{})); err == nil {
		t.Fatal("expected unauthenticated error with no owner in ctx")
	}
}

func TestDeleteSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	makeSpawn(t, s, "sp2", "alice")
	ctx := auth.WithOwner(context.Background(), "alice")

	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.DeleteSpawn(bob, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "sp1"})); err == nil ||
		connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign delete: want PermissionDenied, got %v", err)
	}
	// unknown -> NotFound
	if _, err := s.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "nope"})); err == nil ||
		connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown delete: want NotFound, got %v", err)
	}
	// happy: delete sp1
	if _, err := s.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "sp1", DestroyData: true})); err != nil {
		t.Fatal(err)
	}
	resp, _ := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if len(resp.Msg.Spawns) != 1 || resp.Msg.Spawns[0].SpawnId != "sp2" {
		t.Fatalf("after delete, list=%+v want only sp2", resp.Msg.Spawns)
	}
}

func TestRenameSpawn(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	makeSpawn(t, s, "sp2", "alice")
	ctx := auth.WithOwner(context.Background(), "alice")

	// happy: rename sp1
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: "  Renamed  "})); err != nil {
		t.Fatal(err)
	}
	resp, _ := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	var got string
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			got = sm.Name
		}
	}
	if got != "Renamed" {
		t.Fatalf("sp1 name=%q want %q (trimmed)", got, "Renamed")
	}

	// duplicate names are allowed
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp2", Name: "Renamed"})); err != nil {
		t.Fatalf("duplicate rename must be allowed, got %v", err)
	}

	// empty name -> InvalidArgument
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: "   "})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty name: want InvalidArgument, got %v", err)
	}

	// too long (>80 runes) -> InvalidArgument
	long := ""
	for i := 0; i < 81; i++ {
		long += "x"
	}
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: long})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("long name: want InvalidArgument, got %v", err)
	}

	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.RenameSpawn(bob, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: "x"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign rename: want PermissionDenied, got %v", err)
	}

	// unknown -> NotFound
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "nope", Name: "x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown rename: want NotFound, got %v", err)
	}

	// unauthenticated -> Unauthenticated
	if _, err := s.RenameSpawn(context.Background(), connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: "x"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no owner: want Unauthenticated, got %v", err)
	}
}
