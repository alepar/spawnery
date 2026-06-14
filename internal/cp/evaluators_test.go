package cp

// evaluators_test.go: hermetic unit tests for the CP-side spawn-metric evaluators (§6).
//
// Test matrix:
//   EV1: Over-quota spawn → async suspend (withClaim+suspendLocked) → Suspended.
//   EV2: Idle+detached (last_activity old, no clients) → Suspended.
//   EV3: Idle+attached within the attached budget → no-op (stays Active).
//   EV4: Fresh activity (last_activity recent) → no-op.
//   EV5: Small delta (under quota threshold) → no-op.
//   EV6: PARTITION safety: reconcile with empty running list → no evaluateSpawnMetrics call → stays Active.
//   EV7: De-dup: repeat heartbeat while driver is in-flight → single driver started.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// waitSuspended polls until the spawn reaches 'suspended' or the deadline passes.
func waitSuspended(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, err := s.st.Spawns().Get(context.Background(), id)
		if err == nil && sp.Status == store.Suspended {
			return
		}
		if time.Now().After(deadline) {
			var status store.Status
			if err == nil {
				status = sp.Status
			}
			t.Fatalf("spawn %s: timed out waiting for Suspended, last status=%v err=%v", id, status, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// enableEvaluator configures the server's evaluator policy with short timeouts suitable for tests.
func enableEvaluator(s *Server, idleDetached, idleAttached time.Duration, quotaMB int64) {
	s.SetEvaluatorPolicy(idleDetached, idleAttached, quotaMB)
}

// runningSpawnMsg builds a *nodev1.RunningSpawn for use in evaluateSpawnMetrics calls.
func runningSpawnMsg(spawnID string, deltaBytes, lastActivityUnixMs int64) *nodev1.RunningSpawn {
	return &nodev1.RunningSpawn{
		SpawnId:            spawnID,
		Generation:         1,
		Phase:              nodev1.SpawnPhase_ACTIVE,
		DeltaSizeBytes:     deltaBytes,
		LastActivityUnixMs: lastActivityUnixMs,
	}
}

// EV1: Over-quota spawn → async suspend → Suspended.
func TestEvaluatorOverQuotaSuspends(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "q1"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	enableEvaluator(s, time.Hour, time.Hour, 10) // quota = 10 MiB; no idle

	// Report 20 MiB delta → over-quota.
	const deltaMiB = 20
	rs := runningSpawnMsg("sp1", int64(deltaMiB)<<20, 0)
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	waitSuspended(t, s, "sp1")
	if rt.Bound("sp1") {
		t.Fatal("route must be dropped after evaluator suspend")
	}
}

// EV2: Idle+detached (old last_activity, no clients) → Suspended.
func TestEvaluatorIdleDetachedSuspends(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "i2"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	// idleDetached = 5m; spawn last active 10m ago → idle.
	enableEvaluator(s, 5*time.Minute, time.Hour, 0) // no quota

	lastActivity := time.Now().Add(-10 * time.Minute).UnixMilli()
	rs := runningSpawnMsg("sp1", 0, lastActivity)
	// No clients attached (rt.Attached("sp1") == false after activeSpawnWithRoute without AttachClient).
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	waitSuspended(t, s, "sp1")
	if rt.Bound("sp1") {
		t.Fatal("route must be dropped after idle suspend")
	}
}

// EV3: Idle+attached within the attached budget → no-op (stays Active).
func TestEvaluatorIdleAttachedWithinBudgetNoOp(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "i3"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	// idleDetached=5m, idleAttached=60m; last active 10m ago.
	enableEvaluator(s, 5*time.Minute, 60*time.Minute, 0)

	// Attach a client so rt.Attached("sp1") == true.
	cl := &capClient{}
	if _, err := rt.AttachClient("sp1", "0", "c1", "alice", nil, cl, 0); err != nil {
		t.Fatalf("AttachClient: %v", err)
	}
	defer rt.DetachClient("sp1", "0", "c1")

	lastActivity := time.Now().Add(-10 * time.Minute).UnixMilli()
	rs := runningSpawnMsg("sp1", 0, lastActivity)
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	// Give the (non-existent) goroutine time to start if the test were broken.
	time.Sleep(50 * time.Millisecond)

	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Active {
		t.Fatalf("attached within budget: status=%v want Active", sp.Status)
	}
}

// EV4: Fresh activity (last_activity < idleDetached threshold) → no-op.
func TestEvaluatorFreshActivityNoOp(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "i4"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	enableEvaluator(s, 5*time.Minute, time.Hour, 0)

	// Last activity 1 minute ago → well within the 5m detached budget.
	lastActivity := time.Now().Add(-1 * time.Minute).UnixMilli()
	rs := runningSpawnMsg("sp1", 0, lastActivity)
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	time.Sleep(50 * time.Millisecond)

	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Active {
		t.Fatalf("fresh activity: status=%v want Active", sp.Status)
	}
	_ = rt
}

// EV5: Small delta (under quota threshold) → no-op.
func TestEvaluatorSmallDeltaNoOp(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "q5"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	enableEvaluator(s, time.Hour, time.Hour, 100) // quota = 100 MiB; no idle

	// 5 MiB delta → under 100 MiB threshold.
	rs := runningSpawnMsg("sp1", int64(5)<<20, 0)
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	time.Sleep(50 * time.Millisecond)

	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status != store.Active {
		t.Fatalf("small delta: status=%v want Active", sp.Status)
	}
	_ = rt
}

// EV6: PARTITION safety. reconcileInventory with empty running list → adoptOrStop is never called
// with matched=true → evaluateSpawnMetrics is NEVER called → spawn stays Active.
// This is the structural partition safety guarantee: no heartbeat from node → no metrics → no
// action → spawns preserved.
func TestEvaluatorPartitionNoOp(t *testing.T) {
	s, reg, rt := newTestServer(t)
	sender := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "p6"}}}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	// Enable evaluator with very short idle to ensure it WOULD trigger if called.
	enableEvaluator(s, time.Millisecond, time.Millisecond, 0)

	// Node sends empty running list (partition: pod gone from node's perspective).
	// reconcileInventory with nil/empty running → no adoptOrStop → no evaluateSpawnMetrics.
	s.reconcileInventory(context.Background(), "n1", &capSender{}, nil)

	time.Sleep(50 * time.Millisecond)

	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	// reconcile with empty running marks the spawn Unreachable (unreported active pod on n1),
	// NOT Suspended. The evaluator never ran. This is the expected behavior.
	if sp.Status == store.Suspended {
		t.Fatalf("PARTITION: spawn must NOT be auto-suspended when node reports nothing (no heartbeat); status=%v", sp.Status)
	}
	_ = rt
}

// EV7: De-dup: repeat heartbeat while driver is in-flight → only one driver is started.
// We use a slow suspendSender (long delay) and fire evaluateSpawnMetrics twice; the second
// call must be a no-op (spawn still in flight).
func TestEvaluatorDeDupSingleDriver(t *testing.T) {
	s, reg, rt := newTestServer(t)
	var suspendCount atomic.Int32
	sender := &countingSuspendSender{
		s:       s,
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "dd7"}},
		delay:   200 * time.Millisecond,
		counter: &suspendCount,
	}
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	// quota trigger: 50 MiB threshold, spawn at 100 MiB
	enableEvaluator(s, time.Hour, time.Hour, 50)

	rs := runningSpawnMsg("sp1", int64(100)<<20, 0)
	// First call: starts the async driver.
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	// Brief pause to let the goroutine start and register in evaluatorInFlight.
	time.Sleep(10 * time.Millisecond)

	// Second call: must be a no-op because the driver is in-flight.
	s.evaluateSpawnMetrics(context.Background(), "sp1", rs)

	waitSuspended(t, s, "sp1")

	// Exactly one Suspend must have been sent to the node.
	if n := int(suspendCount.Load()); n != 1 {
		t.Fatalf("de-dup: %d Suspend messages sent, want exactly 1", n)
	}
	_ = rt
}

// countingSuspendSender is a suspendSender variant that counts how many Suspend messages it
// receives. Used by EV7 to assert the de-dup guard fires correctly.
type countingSuspendSender struct {
	s       *Server
	markers []*nodev1.MountMarker
	delay   time.Duration
	counter *atomic.Int32
}

func (c *countingSuspendSender) Send(m *nodev1.CPMessage) error {
	sp := m.GetSuspend()
	if sp == nil {
		return nil
	}
	c.counter.Add(1)
	gen := sp.GetGeneration()
	go func() {
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		c.s.suspends.deliver(&nodev1.SuspendComplete{
			SpawnId: sp.GetSpawnId(), Generation: gen, Markers: c.markers,
		})
	}()
	return nil
}

// Ensure countingSuspendSender satisfies registry.NodeSender.
var _ registry.NodeSender = (*countingSuspendSender)(nil)
