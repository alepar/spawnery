package cp

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
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
		// acked tracks how many StartSpawn messages we have already acked per spawn id, so that a
		// second Provision call for the same spawn id (e.g. ResumeSpawn) is also acked.
		acked := map[string]int{}
		for {
			select {
			case <-stop:
				return
			default:
			}
			var ids []string
			sender.mu.Lock()
			counts := map[string]int{}
			for _, m := range sender.sent {
				if st := m.GetStart(); st != nil {
					counts[st.GetSpawnId()]++
				}
			}
			for id, total := range counts {
				for acked[id] < total {
					acked[id]++
					ids = append(ids, id)
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
	waitActive(t, s, id) // async CreateSpawn — wait for active before suspending/resuming

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

	// unauthenticated -> Unauthenticated
	if _, err := s.SuspendSpawn(context.Background(), connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no owner: want Unauthenticated, got %v", err)
	}
}

// waitActive polls the store until the spawn reaches active (CreateSpawn is async now). Fails on
// timeout or if the spawn errors first.
func waitActive(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, err := s.st.Spawns().Get(ctx, id)
		if err == nil && sp.Status == store.Active {
			return
		}
		if err == nil && sp.Status == store.Errored {
			t.Fatalf("spawn %s errored while waiting for active", id)
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s not active within 3s", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCreateSpawnIsAsyncReturnsStarting(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1}) // present but we don't ack yet
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if err != nil || sp.Status != store.Starting {
		t.Fatalf("status=%v err=%v want starting (async create)", sp.Status, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sender.firstStart() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no StartSpawn was sent")
		}
		time.Sleep(time.Millisecond)
	}
	s.sched.OnStatus(resp.Msg.SpawnId, nodev1.SpawnPhase_ACTIVE)
	waitActive(t, s, resp.Msg.SpawnId)
}

func TestCreateSpawnProvisionFailureSetsError(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ERROR)
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
		if sp.Status == store.Errored {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s not errored after provision failure (status=%v)", resp.Msg.SpawnId, sp.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestResumeSpawn(t *testing.T) {
	s, reg, _ := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id) // async CreateSpawn — wait for active before suspending/resuming

	// resume of an ACTIVE spawn -> FailedPrecondition
	if _, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("resume active: want FailedPrecondition, got %v", err)
	}

	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	if err := s.st.Spawns().SetModel(ctx, id, "m2"); err != nil { // model_applied=false while suspended
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("ResumeSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, id)
	if sp.Status != store.Active {
		t.Fatalf("status=%v want active after resume", sp.Status)
	}
	if !sp.ModelApplied {
		t.Fatalf("after resume: model_applied=false, want true (fresh pod runs spawns.model)")
	}
	c, ok, _ := s.st.Spawns().LiveContainer(ctx, id)
	if !ok || c.Generation != 2 {
		t.Fatalf("resume must start a new-generation container: ok=%v c=%+v want gen 2", ok, c)
	}

	// foreign owner -> PermissionDenied
	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.ResumeSpawn(bob, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign resume: want PermissionDenied, got %v", err)
	}

	// unknown -> NotFound
	if _, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown resume: want NotFound, got %v", err)
	}

	// unauthenticated -> Unauthenticated
	if _, err := s.ResumeSpawn(context.Background(), connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no owner: want Unauthenticated, got %v", err)
	}
}

func TestRecreateSpawn(t *testing.T) {
	s, reg, _ := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id)

	// recreate of an ACTIVE spawn -> FailedPrecondition (only unreachable/errored may recreate).
	if _, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("recreate active: want FailedPrecondition, got %v", err)
	}

	// Simulate node loss -> unreachable, then recreate -> active at a fresh generation.
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}
	if err := s.st.Spawns().SetModel(ctx, id, "m2"); err != nil { // model_applied=false while unreachable
		t.Fatalf("SetModel: %v", err)
	}
	if _, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("RecreateSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, id)
	if sp.Status != store.Active {
		t.Fatalf("status=%v want active after recreate", sp.Status)
	}
	if !sp.ModelApplied {
		t.Fatalf("after recreate: model_applied=false, want true (fresh pod runs spawns.model)")
	}
	c, ok, _ := s.st.Spawns().LiveContainer(ctx, id)
	if !ok || c.Generation != 2 {
		t.Fatalf("recreate must start a new-generation container: ok=%v c=%+v want gen 2", ok, c)
	}

	bob := auth.WithOwner(context.Background(), "bob")
	if _, err := s.RecreateSpawn(bob, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("foreign recreate: want PermissionDenied, got %v", err)
	}
	if _, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown recreate: want NotFound, got %v", err)
	}
}

func TestReconcileInventory(t *testing.T) {
	s, reg, rt := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id) // bound to node "n1" by the acker

	// The node still reports the spawn -> it stays active, and no Stop is sent.
	sender := &capSender{}
	s.reconcileInventory(ctx, "n1", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Active {
		t.Fatalf("reported spawn must stay active, got %v", sp.Status)
	}
	if got := sender.stops(); len(got) != 0 {
		t.Fatalf("reported live spawn must not get a Stop, got %v", got)
	}

	// The node no longer reports it (restart / pod died) -> unreachable, route dropped.
	s.reconcileInventory(ctx, "n1", sender, nil)
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Unreachable {
		t.Fatalf("unreported active spawn must be marked unreachable, got %v", sp.Status)
	}
	if rt.Bound(id) {
		t.Fatal("unreachable spawn must have its route dropped")
	}
}

// A returning node's inventory re-adopts an unreachable spawn: status flips back to active, the
// route is rebound, the live row keeps its generation, and NO Stop is sent. Idempotent across the
// per-heartbeat repeat.
func TestReconcileInventoryAdopt(t *testing.T) {
	s, reg, rt := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id)

	// Simulate node loss (or a CP-restart boot sweep): route dropped, spawn unreachable, live row kept.
	rt.Drop(id)
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}

	sender := &capSender{}
	for i := 0; i < 2; i++ { // twice: re-adopt, then the steady-state heartbeat no-op
		s.reconcileInventory(ctx, "n1", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
		if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Active {
			t.Fatalf("pass %d: adopted spawn must be active, got %v", i, sp.Status)
		}
		if !rt.Bound(id) {
			t.Fatalf("pass %d: adopted spawn must have its route rebound", i)
		}
		if got := sender.stops(); len(got) != 0 {
			t.Fatalf("pass %d: adopted spawn must not get a Stop, got %v", i, got)
		}
	}
	if c, ok, _ := s.st.Spawns().LiveContainer(ctx, id); !ok || c.Generation != 1 || c.NodeID != "n1" {
		t.Fatalf("live row after adopt: ok=%v c=%+v want gen 1 on n1", ok, c)
	}
}

// A node coming back under a DIFFERENT id (or a binding the CP recorded stale) still adopts: the
// live row's node_id is rebound to the reporter.
func TestReconcileInventoryAdoptRebindsNodeID(t *testing.T) {
	s, reg, rt := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id) // live row bound to n1

	rt.Drop(id)
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}

	sender := &capSender{}
	s.reconcileInventory(ctx, "n2", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Active {
		t.Fatalf("adopted spawn must be active, got %v", sp.Status)
	}
	c, ok, _ := s.st.Spawns().LiveContainer(ctx, id)
	if !ok || c.NodeID != "n2" {
		t.Fatalf("live row must be rebound to the reporter: ok=%v c=%+v want node n2", ok, c)
	}
	if !rt.Bound(id) {
		t.Fatal("adopted spawn must have its route bound")
	}
	if got := sender.stops(); len(got) != 0 {
		t.Fatalf("adopted spawn must not get a Stop, got %v", got)
	}
}

// A superseded-generation report (stale pod from before a recreate) is an orphan: the reporting
// node gets StopSpawn with the OLD generation; the current episode is untouched.
func TestReconcileInventoryOrphanSupersededGen(t *testing.T) {
	s, reg, _ := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id)

	// Recreate to generation 2 (the gen-1 pod is now stale).
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}
	if _, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: id})); err != nil {
		t.Fatalf("RecreateSpawn: %v", err)
	}

	sender := &capSender{}
	s.reconcileInventory(ctx, "n2", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
	stops := sender.stops()
	if len(stops) != 1 || stops[0].GetSpawnId() != id || stops[0].GetGeneration() != 1 {
		t.Fatalf("superseded gen must get StopSpawn(id, 1) at the reporter, got %v", stops)
	}
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Active {
		t.Fatalf("current episode must be untouched, got %v", sp.Status)
	}
	if c, ok, _ := s.st.Spawns().LiveContainer(ctx, id); !ok || c.Generation != 2 {
		t.Fatalf("live row must stay at gen 2: ok=%v c=%+v", ok, c)
	}
}

// Suspended and deleted spawns reported running are orphans -> StopSpawn to the reporting node.
func TestReconcileInventoryOrphanSuspendedAndDeleted(t *testing.T) {
	s, reg, _ := newTestServer(t)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	mk := func() string {
		t.Helper()
		resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
		if err != nil {
			t.Fatalf("CreateSpawn: %v", err)
		}
		waitActive(t, s, resp.Msg.SpawnId)
		return resp.Msg.SpawnId
	}
	suspended, deleted := mk(), mk()
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: suspended})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	if _, err := s.StopSpawn(ctx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: deleted})); err != nil {
		t.Fatalf("StopSpawn: %v", err)
	}

	sender := &capSender{}
	s.reconcileInventory(ctx, "n1", sender, []*nodev1.RunningSpawn{
		{SpawnId: suspended, Generation: 1},
		{SpawnId: deleted, Generation: 1},
	})
	stops := sender.stops()
	if len(stops) != 2 {
		t.Fatalf("want 2 StopSpawns (suspended + deleted), got %v", stops)
	}
	got := map[string]uint64{}
	for _, st := range stops {
		got[st.GetSpawnId()] = st.GetGeneration()
	}
	if got[suspended] != 1 || got[deleted] != 1 {
		t.Fatalf("StopSpawns must target both orphans at gen 1, got %v", got)
	}
	if sp, _ := s.st.Spawns().Get(ctx, suspended); sp.Status != store.Suspended {
		t.Fatalf("suspended spawn must stay suspended, got %v", sp.Status)
	}
}

// --- non-conflict store-error paths of adoptOrStop -------------------------

// faultySpawns wraps a SpawnRepo to inject non-conflict failures (DB I/O) into the inventory arms.
type faultySpawns struct {
	store.SpawnRepo
	adoptErr error
	reachErr error
}

func (f *faultySpawns) Adopt(ctx context.Context, id, nodeID string, gen int64) error {
	if f.adoptErr != nil {
		return f.adoptErr
	}
	return f.SpawnRepo.Adopt(ctx, id, nodeID, gen)
}

func (f *faultySpawns) MarkReachable(ctx context.Context, id string, gen int64) error {
	if f.reachErr != nil {
		return f.reachErr
	}
	return f.SpawnRepo.MarkReachable(ctx, id, gen)
}

type faultyStore struct {
	store.Store
	spawns *faultySpawns
}

func (f *faultyStore) Spawns() store.SpawnRepo { return f.spawns }

// newFaultyServer is newTestServer with Adopt/MarkReachable failure injection (set at construction;
// nothing else calls those methods, so the fields need no locking).
func newFaultyServer(t *testing.T, adoptErr, reachErr error) (*Server, *registry.Registry, *router.Router) {
	t.Helper()
	reg := registry.New()
	rt := router.New()
	sc := scheduler.New(reg, rt, time.Second)
	st := store.NewTestStore(t)
	if err := Seed(context.Background(), st, map[string]string{"dev-token": "alice"},
		[]AppSeed{{ID: "secret-app", Ref: "examples/secret-app", Version: "1.0.0", Mounts: []string{"main"}}}); err != nil {
		t.Fatal(err)
	}
	fst := &faultyStore{Store: st, spawns: &faultySpawns{SpawnRepo: st.Spawns(), adoptErr: adoptErr, reachErr: reachErr}}
	s := NewServer(reg, rt, sc, fst, telemetry.NopSink{})
	return s, reg, rt
}

// A non-conflict Adopt failure (DB I/O) says nothing about orphanhood: the reported pod must NOT
// get a Stop — the error is logged and the next heartbeat retries the adopt.
func TestReconcileInventoryAdoptStoreErrorDoesNotStop(t *testing.T) {
	s, reg, rt := newFaultyServer(t, errors.New("db down"), nil)
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id) // live row bound to n1

	rt.Drop(id)
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}

	// Report from n2 -> the rebind path hits the failing Adopt.
	sender := &capSender{}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	s.reconcileInventory(ctx, "n2", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
	log.SetOutput(os.Stderr)

	if got := sender.stops(); len(got) != 0 {
		t.Fatalf("Adopt store error must not stop the reported pod, got %v", got)
	}
	if rt.Bound(id) {
		t.Fatal("Adopt store error must not bind the route")
	}
	if c, ok, _ := s.st.Spawns().LiveContainer(ctx, id); !ok || c.NodeID != "n1" {
		t.Fatalf("live row must be untouched: ok=%v c=%+v want node n1", ok, c)
	}
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Unreachable {
		t.Fatalf("spawn must stay unreachable, got %v", sp.Status)
	}
	if !strings.Contains(logs.String(), "Adopt spawn "+id) {
		t.Fatalf("Adopt store error must be logged, got %q", logs.String())
	}
}

// A non-conflict MarkReachable failure (DB I/O) is logged; behavior is otherwise unchanged
// (route rebound, no Stop, the unreachable->active flip simply did not land).
func TestReconcileInventoryMarkReachableStoreErrorLogged(t *testing.T) {
	s, reg, rt := newFaultyServer(t, nil, errors.New("db down"))
	stop := startAcker(t, s, reg)
	defer stop()
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	id := resp.Msg.SpawnId
	waitActive(t, s, id)

	rt.Drop(id)
	if _, err := s.st.Spawns().MarkUnreachable(ctx, []string{id}); err != nil {
		t.Fatalf("MarkUnreachable: %v", err)
	}

	sender := &capSender{}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	s.reconcileInventory(ctx, "n1", sender, []*nodev1.RunningSpawn{{SpawnId: id, Generation: 1}})
	log.SetOutput(os.Stderr)

	if got := sender.stops(); len(got) != 0 {
		t.Fatalf("MarkReachable store error must not stop the reported pod, got %v", got)
	}
	if !rt.Bound(id) {
		t.Fatal("route must still be rebound on MarkReachable store error")
	}
	if sp, _ := s.st.Spawns().Get(ctx, id); sp.Status != store.Unreachable {
		t.Fatalf("failed flip must leave the spawn unreachable, got %v", sp.Status)
	}
	if !strings.Contains(logs.String(), "MarkReachable spawn "+id) {
		t.Fatalf("MarkReachable store error must be logged, got %q", logs.String())
	}
}
