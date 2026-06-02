package cp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
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
	resp2, _ := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	var got2 string
	for _, sm := range resp2.Msg.Spawns {
		if sm.SpawnId == "sp2" {
			got2 = sm.Name
		}
	}
	if got2 != "Renamed" {
		t.Fatalf("sp2 name=%q want %q (duplicate allowed + persisted)", got2, "Renamed")
	}

	// empty name -> InvalidArgument
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: "   "})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty name: want InvalidArgument, got %v", err)
	}

	// too long (>80 runes) -> InvalidArgument
	long := strings.Repeat("x", 81)
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: long})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("long name: want InvalidArgument, got %v", err)
	}
	// exactly 80 runes -> allowed (boundary)
	if _, err := s.RenameSpawn(ctx, connect.NewRequest(&cpv1.RenameSpawnRequest{SpawnId: "sp1", Name: strings.Repeat("x", 80)})); err != nil {
		t.Fatalf("80-rune name: want success, got %v", err)
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

// startAcker registers node "n1" and spins a goroutine that acks every StartSpawn it sees as
// ACTIVE, so multiple Provision calls (create AND resume) all complete. Returns a stop func.
func startAcker(t *testing.T, s *Server, reg *registry.Registry) func() {
	t.Helper()
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 10, Free: 10})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seen := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			default:
			}
			var ids []string
			sender.mu.Lock()
			for _, m := range sender.sent {
				if st := m.GetStart(); st != nil && !seen[st.GetSpawnId()] {
					seen[st.GetSpawnId()] = true
					ids = append(ids, st.GetSpawnId())
				}
			}
			sender.mu.Unlock()
			for _, id := range ids {
				s.sched.OnStatus(id, nodev1.SpawnPhase_ACTIVE)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() { close(stop); wg.Wait() }
}

func TestSuspendSpawn(t *testing.T) {
	s, reg, _ := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId

	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, id)
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want suspended", sp.Status)
	}
	if _, ok, _ := s.st.Spawns().LiveContainer(ctx, id); ok {
		t.Fatal("suspended spawn must have no live container")
	}

	// suspend a non-active spawn -> FailedPrecondition (a fresh makeSpawn is status=starting)
	makeSpawn(t, s, "starting1", "alice")
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "starting1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("suspend non-active: want FailedPrecondition, got %v", err)
	}

	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.SuspendSpawn(bob, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign suspend: want PermissionDenied, got %v", err)
	}

	// unknown -> NotFound
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown suspend: want NotFound, got %v", err)
	}
}
