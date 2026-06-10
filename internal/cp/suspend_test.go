package cp

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/store"
)

// suspendSender is a fake NodeSender for the marker-protocol SuspendSpawn flow: on a Suspend it
// records the request and (unless drop) asynchronously delivers a SuspendComplete into the server's
// suspend-waiter registry — the per-mount markers, gen override, and delay are all configurable so a
// test can exercise the happy path, an await timeout, and a stale-episode reply.
type suspendSender struct {
	s        *Server
	markers  []*nodev1.MountMarker
	replyGen *uint64 // if set, the gen echoed in SuspendComplete (else echo the request's)
	drop     bool    // if true, never reply (forces the await to time out)
	delay    time.Duration

	mu         sync.Mutex
	gotSuspend bool
	lastGen    uint64
}

func (a *suspendSender) Send(m *nodev1.CPMessage) error {
	sp := m.GetSuspend()
	if sp == nil {
		return nil
	}
	a.mu.Lock()
	a.gotSuspend = true
	a.lastGen = sp.GetGeneration()
	a.mu.Unlock()
	if a.drop {
		return nil
	}
	gen := sp.GetGeneration()
	if a.replyGen != nil {
		gen = *a.replyGen
	}
	go func() {
		if a.delay > 0 {
			time.Sleep(a.delay)
		}
		a.s.suspends.deliver(&nodev1.SuspendComplete{SpawnId: sp.GetSpawnId(), Generation: gen, Markers: a.markers})
	}()
	return nil
}

// activeSpawnWithRoute seeds an active spawn (gen-1 live container) on connected node "n1" with its
// route bound to sender, so SuspendOnNode reaches sender. The spawn has one "main" mount (so
// per-mount markers have a row to land on).
func activeSpawnWithRoute(t *testing.T, s *Server, reg *registry.Registry, rt *router.Router, id, owner string, sender registry.NodeSender) {
	t.Helper()
	ctx := context.Background()
	if err := s.st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: 1}); err != nil {
		t.Fatalf("seed owner %s: %v", owner, err)
	}
	sp := store.Spawn{
		ID: id, OwnerID: owner, AppID: "secret-app", AppVersion: "1.0.0", AppRef: "examples/secret-app",
		Model: "m", Status: store.Starting, CreatedAt: 1, LastUsedAt: 1,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		return tx.Spawns().Create(ctx, sp, []store.Mount{{Name: "main", BackendURI: "scratch"}})
	}); err != nil {
		t.Fatalf("seed spawn %s: %v", id, err)
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().SetActive(ctx, id, "n1", 1) }); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	rt.Bind(id, "n1", sender)
}

// Happy path: SuspendSpawn asks the node to persist+tear down (generation-fenced), awaits the
// SuspendComplete, records the per-mount markers, drops the route, and finalizes 'suspended'.
func TestSuspendSpawnRecordsMarkers(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "marker-xyz"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	if !sender.gotSuspend || sender.lastGen != 1 {
		t.Fatalf("node got suspend=%v gen=%d, want true/1", sender.gotSuspend, sender.lastGen)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want suspended", sp.Status)
	}
	if _, ok, _ := s.st.Spawns().LiveContainer(ctx, "sp1"); ok {
		t.Fatal("suspended spawn must have no live container")
	}
	if rt.Bound("sp1") {
		t.Fatal("route must be dropped after suspend")
	}
	mounts, err := s.st.Spawns().GetMounts(ctx, "sp1")
	if err != nil || len(mounts) != 1 {
		t.Fatalf("GetMounts = %+v err=%v, want one mount", mounts, err)
	}
	if mounts[0].PersistMarker != "marker-xyz" {
		t.Fatalf("persist_marker = %q, want marker-xyz (recorded from SuspendComplete)", mounts[0].PersistMarker)
	}
}

// Await timeout: a node that never replies SuspendComplete (slow/wedged/unreachable) moves the spawn
// to terminal 'error' (design §5: "persist failure → error") rather than leaving it in 'suspending'.
func TestSuspendSpawnAwaitTimeoutErrors(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.suspendTimeout = 50 * time.Millisecond
	sender := &suspendSender{drop: true}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("want DeadlineExceeded on suspend timeout, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want error after suspend timeout", sp.Status)
	}
}

// A SuspendComplete whose generation differs from the awaiting episode's (a stale reply from a
// superseded pod) is dropped: the await sees no in-episode reply and times out -> error. Proves the
// generation fence on the inbound SuspendComplete.
func TestSuspendSpawnStaleSuspendCompleteIgnored(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.suspendTimeout = 80 * time.Millisecond
	staleGen := uint64(99)
	sender := &suspendSender{replyGen: &staleGen, markers: []*nodev1.MountMarker{{Name: "main", Marker: "stale"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("stale-gen SuspendComplete must be ignored -> timeout, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want error", sp.Status)
	}
	// The stale marker must NOT have been recorded.
	mounts, _ := s.st.Spawns().GetMounts(ctx, "sp1")
	if len(mounts) == 1 && mounts[0].PersistMarker == "stale" {
		t.Fatal("stale-episode marker must not be recorded")
	}
}

// suspendWaiters.deliver routes a matching (spawn, gen) reply and drops both an unknown-spawn reply
// and a generation mismatch (the stale-episode fence), without blocking.
func TestSuspendWaitersDeliverGenerationFence(t *testing.T) {
	w := newSuspendWaiters()
	ch := w.register("sp1", 3)
	defer w.unregister("sp1")

	w.deliver(&nodev1.SuspendComplete{SpawnId: "other", Generation: 3}) // unknown spawn -> dropped
	w.deliver(&nodev1.SuspendComplete{SpawnId: "sp1", Generation: 2})   // stale gen -> dropped
	select {
	case got := <-ch:
		t.Fatalf("dropped replies must not be delivered, got %+v", got)
	default:
	}

	w.deliver(&nodev1.SuspendComplete{SpawnId: "sp1", Generation: 3, Markers: []*nodev1.MountMarker{{Name: "main", Marker: "m"}}})
	select {
	case got := <-ch:
		if got.GetGeneration() != 3 || len(got.GetMarkers()) != 1 {
			t.Fatalf("delivered = %+v, want gen 3 with 1 marker", got)
		}
	default:
		t.Fatal("matching reply was not delivered")
	}
}
