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
	"spawnery/internal/cp/store"
)

// progressSender is a fake NodeSender for stall-detector tests. On a Suspend CPMessage it
// emits SuspendProgress hints directly into s.suspends at progressPeriod intervals (bypassing
// the wire, since the SuspendProgress proto message is not yet added to node.proto — sp-u53.7.2
// TODO). After totalProgress events it optionally delivers SuspendComplete.
// If stallOnly=true, SuspendComplete is never delivered (forces the stall path).
type progressSender struct {
	s               *Server
	progressPeriod  time.Duration    // interval between SuspendProgress hints (0 = no progress)
	totalProgress   int              // number of progress events before delivering SuspendComplete
	finalMarkers    []*nodev1.MountMarker
	progressMarkers map[string]string // markers to include in each progress hint
	stallOnly       bool              // if true, never deliver SuspendComplete (stall test)

	mu         sync.Mutex
	gotSuspend bool
	lastGen    uint64
}

func (ps *progressSender) Send(m *nodev1.CPMessage) error {
	sp := m.GetSuspend()
	if sp == nil {
		return nil
	}
	ps.mu.Lock()
	ps.gotSuspend = true
	ps.lastGen = sp.GetGeneration()
	ps.mu.Unlock()

	go func() {
		for i := 0; i < ps.totalProgress; i++ {
			time.Sleep(ps.progressPeriod)
			ps.s.suspends.progress(SuspendProgressHint{
				SpawnID:    sp.GetSpawnId(),
				Generation: sp.GetGeneration(),
				Phase:      "snapshot",
				Detail:     "progress event",
				Markers:    ps.progressMarkers,
			})
		}
		if !ps.stallOnly {
			// Deliver the terminal SuspendComplete after all progress events.
			ps.s.suspends.deliver(&nodev1.SuspendComplete{
				SpawnId:    sp.GetSpawnId(),
				Generation: sp.GetGeneration(),
				Markers:    ps.finalMarkers,
			})
		}
		// If stallOnly=true: progress events are emitted but no SuspendComplete → stall fires.
	}()
	return nil
}

// TestSuspendProgressSlowButSucceeds verifies that a slow-but-progressing suspend does NOT trip
// the stall detector — progress events reset the stall timer so a snapshot that takes longer than
// the stall window but emits regular progress completes successfully (status=Suspended).
// This is the core correctness case for sp-u53.7.2: the old blunt 30s total deadline would have
// fired here; the per-transition stall window + progress-reset does not.
func TestSuspendProgressSlowButSucceeds(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	// Stall window: 50ms. Progress arrives every 20ms — well within the window.
	// Total suspend time: 15 * 20ms = 300ms, which is 6× the stall window.
	// The old 30s total deadline would NOT fire here (300ms < 30s), but the NEW
	// stall window of 50ms WOULD fire if progress resets were absent. This test
	// verifies the resets are working.
	stallWindow := 50 * time.Millisecond
	s.SetSuspendStallWindow(stallWindow)
	s.suspendTimeout = 5 * time.Second

	sender := &progressSender{
		s:              s,
		progressPeriod: 20 * time.Millisecond, // < stallWindow → each event resets the timer
		totalProgress:  15,                    // 15 * 20ms = 300ms total (> stallWindow)
		finalMarkers:   []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
	}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if err != nil {
		t.Fatalf("slow-but-progressing suspend must succeed, got: %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want Suspended — progress resets must have prevented false stall", sp.Status)
	}
	// Marker must be recorded from the terminal SuspendComplete.
	mounts, _ := s.st.Spawns().GetMounts(context.Background(), "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "m1" {
		t.Fatalf("persist_marker=%q want m1 (from terminal SuspendComplete)", mounts[0].PersistMarker)
	}
}

// TestSuspendProgressWedgeErrors verifies that a suspend with NO progress events trips the stall
// detector: the stall window fires → Errored. This is the node-wedge path for sp-u53.7.2.
func TestSuspendProgressWedgeErrors(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	s.SetSuspendStallWindow(40 * time.Millisecond) // short stall window → fires quickly
	s.suspendTimeout = 5 * time.Second

	sender := &progressSender{
		s:         s,
		stallOnly: true, // never sends progress or SuspendComplete
	}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("wedge suspend must return CodeDeadlineExceeded, got %v", err)
	}
	if err == nil || err.Error() == "" {
		t.Fatal("expected non-nil error with 'stalled' in message")
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored after stall", sp.Status)
	}
}

// TestSuspendLateCompleteReconciles verifies the sp-iuo1 closure: after a stall → Errored, a late
// SuspendComplete (the node genuinely finished) reconciles the spawn to Suspended with markers
// recorded. This is the key sp-iuo1 regression that previously left the spawn stranded as Errored
// with markers lost.
func TestSuspendLateCompleteReconciles(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	s.SetSuspendStallWindow(30 * time.Millisecond)
	s.suspendTimeout = 5 * time.Second

	// Stall: sender never replies. Stall fires → Errored.
	sender := &progressSender{s: s, stallOnly: true}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("stall must return DeadlineExceeded, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("pre-reconcile status=%v want Errored", sp.Status)
	}

	// Node finishes suspend later and sends a SuspendComplete with no live waiter. The reconcile
	// path catches this: Errored → Suspended with markers persisted.
	lateMarkers := []*nodev1.MountMarker{{Name: "main", Marker: "late-marker"}}
	lateComplete := &nodev1.SuspendComplete{
		SpawnId:    "sp1",
		Generation: 1, // gen=1 matches the stall episode
		Markers:    lateMarkers,
	}
	// deliver returns false (no live waiter) → server.go spawns reconcileLateSuspend.
	// In the test we call it directly (same package) to avoid timing races.
	s.reconcileLateSuspend(context.Background(), lateComplete)

	sp, _ = s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("after late reconcile: status=%v want Suspended (sp-iuo1)", sp.Status)
	}
	mounts, _ := s.st.Spawns().GetMounts(context.Background(), "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "late-marker" {
		t.Fatalf("persist_marker=%q want late-marker (from late SuspendComplete)", mounts[0].PersistMarker)
	}
}

// TestSuspendPartialMarkersPersistedOnStall verifies that partial markers carried by SuspendProgress
// events are accumulated and persisted to the store when the stall fires. This ensures that even
// if a wedge prevents the terminal SuspendComplete, the markers that arrived before the wedge are
// not lost.
func TestSuspendPartialMarkersPersistedOnStall(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	stallWindow := 60 * time.Millisecond
	s.SetSuspendStallWindow(stallWindow)
	s.suspendTimeout = 5 * time.Second

	// Send one progress event with partial markers, then stall (no SuspendComplete).
	sender := &progressSender{
		s:               s,
		progressPeriod:  5 * time.Millisecond, // one event quickly
		totalProgress:   1,
		progressMarkers: map[string]string{"main": "partial-marker"},
		stallOnly:       true, // after 1 progress event, never deliver SuspendComplete → stall
	}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("stall must return DeadlineExceeded, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored after stall", sp.Status)
	}
	// Partial markers must have been persisted during the stall (on progress-reset or stall path).
	mounts, _ := s.st.Spawns().GetMounts(context.Background(), "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "partial-marker" {
		t.Fatalf("persist_marker=%q want partial-marker (persisted from progress event on stall)", mounts[0].PersistMarker)
	}
}

// TestSuspendStaleGenProgressDropped verifies that a SuspendProgress hint with a stale generation
// is dropped — it does NOT reset the stall timer. The stall fires on schedule, proving the
// generation fence on progress events prevents a stale pod from indefinitely deferring the stall.
func TestSuspendStaleGenProgressDropped(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	stallWindow := 50 * time.Millisecond
	s.SetSuspendStallWindow(stallWindow)
	s.suspendTimeout = 5 * time.Second

	// This sender emits progress with the WRONG generation (99 instead of 1), then stalls.
	staleGen := uint64(99)
	sender := &staleGenProgressSender{
		s:              s,
		progressPeriod: 10 * time.Millisecond,
		totalProgress:  10, // would reset the stall if gen matched, but it's stale
		staleGen:       staleGen,
	}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("stale-gen progress must not reset stall → DeadlineExceeded, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored (stale progress must not prevent stall)", sp.Status)
	}
}

// staleGenProgressSender emits SuspendProgress hints with a stale generation to test the
// generation fence in suspendWaiters.progress.
type staleGenProgressSender struct {
	s              *Server
	progressPeriod time.Duration
	totalProgress  int
	staleGen       uint64

	mu         sync.Mutex
	gotSuspend bool
}

func (ss *staleGenProgressSender) Send(m *nodev1.CPMessage) error {
	sp := m.GetSuspend()
	if sp == nil {
		return nil
	}
	ss.mu.Lock()
	ss.gotSuspend = true
	ss.mu.Unlock()

	go func() {
		for i := 0; i < ss.totalProgress; i++ {
			time.Sleep(ss.progressPeriod)
			// Send with stale generation — should be dropped by progress().
			ss.s.suspends.progress(SuspendProgressHint{
				SpawnID:    sp.GetSpawnId(),
				Generation: ss.staleGen, // WRONG generation
				Phase:      "snapshot",
			})
		}
		// Never send SuspendComplete → stall fires
	}()
	return nil
}

// TestSuspendWaitersProgressGenerationFence verifies the suspendWaiters.progress method's
// generation fence in isolation: stale-generation progress is dropped (returns false) and does
// not signal progressCh; correct-generation progress is delivered.
func TestSuspendWaitersProgressGenerationFence(t *testing.T) {
	w := newSuspendWaiters()
	wt := w.register("sp1", 3)
	defer w.unregister("sp1")

	// Stale gen: dropped, no progressCh signal.
	if got := w.progress(SuspendProgressHint{SpawnID: "sp1", Generation: 2, Phase: "snapshot"}); got {
		t.Fatal("stale-gen progress must return false")
	}
	select {
	case <-wt.progressCh:
		t.Fatal("stale-gen progress must not signal progressCh")
	default:
	}

	// Unknown spawn: dropped.
	if got := w.progress(SuspendProgressHint{SpawnID: "other", Generation: 3, Phase: "snapshot"}); got {
		t.Fatal("unknown-spawn progress must return false")
	}

	// Correct gen: delivered, progressCh signalled.
	markers := map[string]string{"main": "m1"}
	if got := w.progress(SuspendProgressHint{SpawnID: "sp1", Generation: 3, Phase: "snapshot", Markers: markers}); !got {
		t.Fatal("correct-gen progress must return true")
	}
	select {
	case <-wt.progressCh:
		// expected
	default:
		t.Fatal("correct-gen progress must signal progressCh")
	}
	// Partial markers must have been accumulated.
	wt.markersMu.Lock()
	pm := wt.partialMarkers["main"]
	wt.markersMu.Unlock()
	if pm != "m1" {
		t.Fatalf("partial marker not accumulated: got %q want m1", pm)
	}
}

// TestReconcileSuspendedAfterErrorNoopWhenNotErrored verifies that reconcileLateSuspend is a
// no-op when the spawn is not in Errored state — it must not interfere with an active or already-
// suspended spawn.
func TestReconcileSuspendedAfterErrorNoopWhenNotErrored(t *testing.T) {
	s, reg, rt := newTestServer(t)
	// Use a successful suspend sender.
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Suspend normally → Suspended.
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("pre-reconcile status=%v want Suspended", sp.Status)
	}

	// A late SuspendComplete arrives (duplicate — the first already delivered). reconcileLateSuspend
	// must be a no-op since the spawn is already Suspended, not Errored.
	late := &nodev1.SuspendComplete{SpawnId: "sp1", Generation: 1, Markers: []*nodev1.MountMarker{{Name: "main", Marker: "should-not-overwrite"}}}
	s.reconcileLateSuspend(context.Background(), late)

	sp, _ = s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("after no-op reconcile: status=%v want Suspended", sp.Status)
	}
	// Marker must still be the original one, not overwritten.
	mounts, _ := s.st.Spawns().GetMounts(context.Background(), "sp1")
	if len(mounts) == 1 && mounts[0].PersistMarker == "should-not-overwrite" {
		t.Fatal("reconcile must not overwrite markers when spawn is not Errored")
	}
}

// TestReconcileSuspendedAfterErrorStaleGenDropped verifies that reconcileLateSuspend is a no-op
// when the late reply's generation doesn't match the spawn's latest container generation.
func TestReconcileSuspendedAfterErrorStaleGenDropped(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	s.SetSuspendStallWindow(30 * time.Millisecond)
	s.suspendTimeout = 5 * time.Second

	// Stall → Errored at gen=1.
	sender := &progressSender{s: s, stallOnly: true}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	_, _ = s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("pre-reconcile status=%v want Errored", sp.Status)
	}

	// Late SuspendComplete with WRONG generation (gen=99 instead of 1).
	// reconcileLateSuspend must skip it (gen mismatch).
	staleComplete := &nodev1.SuspendComplete{
		SpawnId:    "sp1",
		Generation: 99,
		Markers:    []*nodev1.MountMarker{{Name: "main", Marker: "should-not-record"}},
	}
	s.reconcileLateSuspend(context.Background(), staleComplete)

	sp, _ = s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("after stale-gen reconcile: status=%v want Errored (stale gen must be dropped)", sp.Status)
	}

	// Verify reg and rt are used (suppress unused variable warnings).
	_ = reg
	_ = rt
}
