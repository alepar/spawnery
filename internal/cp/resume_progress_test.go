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

// resumeProgressSender is a fake NodeSender for resume stall-detector tests. On a Start CPMessage
// it emits ResumeProgressHint events directly into s.resumes at progressPeriod intervals (simulating
// what the real node does: attach.go's startSpawn calls a.resumeProgress at each phase boundary).
// After totalProgress events it optionally calls s.sched.OnStatus(ACTIVE) to complete the resume.
// If stallOnly=true, OnStatus is never called (forces the stall path).
type resumeProgressSender struct {
	s              *Server
	progressPeriod time.Duration
	totalProgress  int
	stallOnly      bool

	mu          sync.Mutex
	gotStart    bool
	lastSpawnID string
	lastGen     uint64
}

func (rs *resumeProgressSender) Send(m *nodev1.CPMessage) error {
	st := m.GetStart()
	if st == nil {
		return nil
	}
	rs.mu.Lock()
	rs.gotStart = true
	rs.lastSpawnID = st.GetSpawnId()
	rs.lastGen = st.GetGeneration()
	gen := st.GetGeneration()
	rs.mu.Unlock()

	go func() {
		for i := 0; i < rs.totalProgress; i++ {
			time.Sleep(rs.progressPeriod)
			rs.s.resumes.progress(ResumeProgressHint{
				SpawnID:    st.GetSpawnId(),
				Generation: gen,
				Phase:      "starting",
				Detail:     "resume progress event",
			})
		}
		if !rs.stallOnly {
			rs.s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ACTIVE, "")
		}
		// If stallOnly=true: no ACTIVE signal → stall fires.
	}()
	return nil
}

// staleGenResumeProgressSender emits ResumeProgressHint events with a stale generation to test the
// generation fence in resumeWaiters.progress. It never calls OnStatus, so the stall fires.
type staleGenResumeProgressSender struct {
	s              *Server
	progressPeriod time.Duration
	totalProgress  int
	staleGen       uint64

	mu       sync.Mutex
	gotStart bool
}

func (ss *staleGenResumeProgressSender) Send(m *nodev1.CPMessage) error {
	st := m.GetStart()
	if st == nil {
		return nil
	}
	ss.mu.Lock()
	ss.gotStart = true
	ss.mu.Unlock()

	go func() {
		for i := 0; i < ss.totalProgress; i++ {
			time.Sleep(ss.progressPeriod)
			// Send with stale generation — must be dropped by resumeWaiters.progress.
			ss.s.resumes.progress(ResumeProgressHint{
				SpawnID:    st.GetSpawnId(),
				Generation: ss.staleGen, // WRONG generation
				Phase:      "starting",
				Detail:     "stale resume progress",
			})
		}
		// Never sends ACTIVE → stall fires
	}()
	return nil
}

// TestResumeProgressSlowButSucceeds verifies that a slow-but-progressing resume does NOT trip the
// stall detector — progress events reset the stall timer so a resume that takes longer than the
// stall window but emits regular progress completes successfully (status=Active).
// This is the resume-side analogue of TestSuspendProgressSlowButSucceeds (sp-u53.7.2).
func TestResumeProgressSlowButSucceeds(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	// Stall window: 50ms. Progress arrives every 20ms — well within the window.
	// Total resume time: 15 * 20ms = 300ms, which is 6× the stall window.
	// Without stall-timer resets the 50ms window would fire on the first gap; with resets it must not.
	stallWindow := 50 * time.Millisecond
	s.SetResumeStallWindow(stallWindow)
	s.resumeTimeout = 5 * time.Second
	s.suspendTimeout = 5 * time.Second

	suspender := &suspendSender{
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
	}
	suspender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", suspender)
	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("initial SuspendSpawn: %v", err)
	}

	// Swap in a resume sender that emits progress every 20ms for 15 events, then ACTIVE.
	resumeSender := &resumeProgressSender{
		s:              s,
		progressPeriod: 20 * time.Millisecond,
		totalProgress:  15,
	}
	addNode(reg, "n1", "", "", 1, resumeSender)

	_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "sp1"}))
	if err != nil {
		t.Fatalf("slow-but-progressing resume must succeed, got: %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Active {
		t.Fatalf("status=%v want Active — progress resets must have prevented false stall", sp.Status)
	}
}

// TestResumeProgressWedgeErrors verifies that a resume with NO progress events trips the stall
// detector: the stall window fires → Errored (revertOnFail=false on plain ResumeSpawn).
func TestResumeProgressWedgeErrors(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	s.SetResumeStallWindow(40 * time.Millisecond)
	s.resumeTimeout = 5 * time.Second
	s.suspendTimeout = 5 * time.Second

	suspender := &suspendSender{
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
	}
	suspender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", suspender)
	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("initial SuspendSpawn: %v", err)
	}

	// Resume sender that receives StartSpawn but never sends progress or ACTIVE → stall fires.
	resumeSender := &resumeProgressSender{s: s, stallOnly: true}
	addNode(reg, "n1", "", "", 1, resumeSender)

	_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("wedge resume must return CodeDeadlineExceeded, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored after stall (revertOnFail=false on plain ResumeSpawn)", sp.Status)
	}
}

// TestMigrateResumeProgressWedgeRevertsToSuspended verifies that a resume stall on the migration
// path (revertOnFail=true) reverts the spawn to Suspended rather than Errored. The user's data
// (persist marker from the initial suspend) must survive the revert.
func TestMigrateResumeProgressWedgeRevertsToSuspended(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	s.SetResumeStallWindow(40 * time.Millisecond)
	s.resumeTimeout = 5 * time.Second
	s.suspendTimeout = 5 * time.Second

	// Source node: suspendSender completes the suspend immediately.
	src := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "safe-marker"}}}
	src.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", src)

	// Target node: has capacity but stalls on resume (never sends progress or ACTIVE).
	stalledResume := &resumeProgressSender{s: s, stallOnly: true}
	addNode(reg, "n2", "cloud", "", 1, stalledResume)

	ctx := auth.WithOwner(context.Background(), "alice")
	_, err := s.MigrateSpawn(ctx, connect.NewRequest(&cpv1.MigrateSpawnRequest{SpawnId: "sp1", TargetNodeId: "n2"}))
	if err == nil {
		t.Fatal("MigrateSpawn must fail when target resume stalls")
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Suspended {
		t.Fatalf("after migrate-resume stall: status=%v want Suspended (revertOnFail=true must revert)", sp.Status)
	}
	// Marker from the initial suspend must be preserved — user's data is safe and resumable.
	mounts, _ := s.st.Spawns().GetMounts(context.Background(), "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "safe-marker" {
		t.Fatalf("mounts=%+v, want main mount with marker safe-marker after revert", mounts)
	}
}

// TestHandleSuspendReplyClean verifies handleSuspendReply on the success path: a clean
// SuspendComplete (no Error field) records the mount marker, drops the route, and finalises the
// spawn as Suspended. Returns the original SuspendComplete and nil error.
func TestHandleSuspendReplyClean(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	bgCtx := context.Background()

	// Manually put the spawn in Suspending state, mirroring what suspendLocked does before calling
	// handleSuspendReply.
	sp, _ := s.st.Spawns().Get(bgCtx, "sp1")
	c, _, _ := s.st.Spawns().LiveContainer(bgCtx, "sp1")
	gen := c.Generation
	leaseID := "test-handle-suspend-lease"
	now := time.Now()
	newSeq, err := s.st.Spawns().Acquire(bgCtx, "sp1", "test-cp", leaseID,
		now.UnixNano(), now.Add(time.Minute).UnixNano(), sp.StatusSeq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.st.Spawns().TransitionClaimed(bgCtx, "sp1", leaseID, newSeq, gen, store.Suspending); err != nil {
		t.Fatalf("TransitionClaimed Active→Suspending: %v", err)
	}

	sc := &nodev1.SuspendComplete{
		SpawnId:    "sp1",
		Generation: uint64(gen),
		Markers:    []*nodev1.MountMarker{{Name: "main", Marker: "reply-marker"}},
	}
	got, err := s.handleSuspendReply(bgCtx, "alice", "sp1", gen, leaseID, sc)
	if err != nil {
		t.Fatalf("clean handleSuspendReply: %v", err)
	}
	if got == nil {
		t.Fatal("clean handleSuspendReply must return non-nil sc")
	}
	spAfter, _ := s.st.Spawns().Get(bgCtx, "sp1")
	if spAfter.Status != store.Suspended {
		t.Fatalf("status=%v want Suspended after clean reply", spAfter.Status)
	}
	mounts, _ := s.st.Spawns().GetMounts(bgCtx, "sp1")
	if len(mounts) != 1 || mounts[0].PersistMarker != "reply-marker" {
		t.Fatalf("mount marker=%q want reply-marker from handleSuspendReply", mounts[0].PersistMarker)
	}
}

// TestHandleSuspendReplyGateError verifies handleSuspendReply on the gate-failure path: an Error-
// set SuspendComplete means the node refused the suspend (spawn is still running). The function
// must return CodeFailedPrecondition and the spawn must revert to Active.
func TestHandleSuspendReplyGateError(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	bgCtx := context.Background()

	// Manually put the spawn in Suspending state.
	sp, _ := s.st.Spawns().Get(bgCtx, "sp1")
	c, _, _ := s.st.Spawns().LiveContainer(bgCtx, "sp1")
	gen := c.Generation
	leaseID := "test-gate-error-lease"
	now := time.Now()
	newSeq, err := s.st.Spawns().Acquire(bgCtx, "sp1", "test-cp", leaseID,
		now.UnixNano(), now.Add(time.Minute).UnixNano(), sp.StatusSeq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.st.Spawns().TransitionClaimed(bgCtx, "sp1", leaseID, newSeq, gen, store.Suspending); err != nil {
		t.Fatalf("TransitionClaimed Active→Suspending: %v", err)
	}

	// Error-set reply: gate failure — node refused the suspend, spawn left running.
	sc := &nodev1.SuspendComplete{
		SpawnId:    "sp1",
		Generation: uint64(gen),
		Error:      "gate failed: criu dump aborted",
	}
	_, err = s.handleSuspendReply(bgCtx, "alice", "sp1", gen, leaseID, sc)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("gate-error reply must return CodeFailedPrecondition, got %v", err)
	}
	spAfter, _ := s.st.Spawns().Get(bgCtx, "sp1")
	if spAfter.Status != store.Active {
		t.Fatalf("status=%v want Active after gate-error revert (spawn left running)", spAfter.Status)
	}
}

// TestResumeStaleGenProgressDropped verifies that a ResumeProgressHint with a stale generation is
// dropped — it does NOT reset the stall timer. The stall fires on schedule, proving the generation
// fence on resume progress events prevents a stale pod from indefinitely deferring the stall.
func TestResumeStaleGenProgressDropped(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second
	stallWindow := 50 * time.Millisecond
	s.SetResumeStallWindow(stallWindow)
	s.resumeTimeout = 5 * time.Second
	s.suspendTimeout = 5 * time.Second

	suspender := &suspendSender{
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
	}
	suspender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", suspender)
	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("initial SuspendSpawn: %v", err)
	}

	// Swap in a sender that emits progress with the WRONG generation (99). These events must be
	// dropped by the generation fence, so the stall window fires on schedule → Errored.
	staleSender := &staleGenResumeProgressSender{
		s:              s,
		progressPeriod: 10 * time.Millisecond,
		totalProgress:  10, // would reset the stall if gen matched, but it's stale
		staleGen:       99,
	}
	addNode(reg, "n1", "", "", 1, staleSender)

	_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("stale-gen progress must not reset stall → DeadlineExceeded, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Errored {
		t.Fatalf("status=%v want Errored (stale progress must not prevent stall)", sp.Status)
	}
	_ = rt
}

// TestResumeWaitersProgressGenerationFence verifies the resumeWaiters.progress method's generation
// fence in isolation: stale-generation progress is dropped (returns false) and does not signal
// progressCh; correct-generation progress is delivered (returns true, progressCh signalled).
func TestResumeWaitersProgressGenerationFence(t *testing.T) {
	w := newResumeWaiters()
	wt := w.register("sp1", 3)
	defer w.unregister("sp1")

	// Stale gen: dropped, no progressCh signal.
	if got := w.progress(ResumeProgressHint{SpawnID: "sp1", Generation: 2, Phase: "starting"}); got {
		t.Fatal("stale-gen progress must return false")
	}
	select {
	case <-wt.progressCh:
		t.Fatal("stale-gen progress must not signal progressCh")
	default:
	}

	// Unknown spawn: dropped.
	if got := w.progress(ResumeProgressHint{SpawnID: "other", Generation: 3, Phase: "starting"}); got {
		t.Fatal("unknown-spawn progress must return false")
	}

	// Correct gen: delivered, progressCh signalled.
	if got := w.progress(ResumeProgressHint{SpawnID: "sp1", Generation: 3, Phase: "starting", Detail: "ok"}); !got {
		t.Fatal("correct-gen progress must return true")
	}
	select {
	case <-wt.progressCh:
		// expected
	default:
		t.Fatal("correct-gen progress must signal progressCh")
	}
}
