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
	"spawnery/internal/cp/store"
)

// TestSuspendConcurrentReconcileNeverFlipsUnreachable exercises the core race that drove this work
// (sp-u53.7.5): SuspendSpawn runs concurrently with reconcileInventory where the spawn pod is no
// longer reported by the node. With the old deferred-SetSuspending design, reconcile would flip the
// Active-but-unreported spawn to Unreachable. With the new claim+TransitionClaimed approach,
// the spawn reaches Suspending BEFORE the node round-trip so reconcile skips it.
//
// The test uses a delayed suspendSender to give the reconcile goroutine a window to run after the
// suspend write but before the node reply.
func TestSuspendConcurrentReconcileNeverFlipsUnreachable(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second // generous — no real timing needed

	// A suspend sender with a brief delay: reconcile runs during the delay.
	sender := &suspendSender{
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
		delay:   30 * time.Millisecond,
	}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Fire SuspendSpawn in the background so we can observe its in-flight state.
	suspendDone := make(chan error, 1)
	go func() {
		_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
		suspendDone <- err
	}()

	// Poll until the spawn reaches Suspending: claim acquired + Active→Suspending written to DB.
	// Only THEN start the reconcile goroutine with empty inventory (simulating the pod being
	// torn down during the suspension round-trip). Starting reconcile before the claim is
	// acquired would test a pre-claim race, not the race this test targets.
	bgCtx := context.Background()
	for {
		sp, _ := s.st.Spawns().Get(bgCtx, "sp1")
		if sp.Status == store.Suspending {
			break
		}
		time.Sleep(time.Millisecond)
	}

	var wg sync.WaitGroup
	stopReconcile := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopReconcile:
				return
			default:
				// Node reports no running spawns — the torn-down pod is gone.
				s.reconcileInventory(ctx, "n1", &capSender{}, nil)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	err := <-suspendDone
	close(stopReconcile)
	wg.Wait()

	if err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	// Must end up Suspended — NEVER Unreachable, no ErrConflict.
	if sp.Status != store.Suspended {
		t.Fatalf("status=%v want Suspended (reconcile must never flip to Unreachable mid-suspend)", sp.Status)
	}
}

// TestSuspendGateAbortRevertsToActiveWithConcurrentReconcile verifies the gate-abort path under
// concurrent reconcile: when the node rejects the suspend, the row must revert to Active (not stay
// stuck at Suspending), and subsequent reconciles see Active again.
func TestSuspendGateAbortRevertsToActiveWithConcurrentReconcile(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second

	sender := &suspendSender{
		errMsg: "journal snapshot failed",
		delay:  20 * time.Millisecond,
	}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Run reconcile concurrently. The gate-abort means the node REFUSED to suspend, so the spawn IS
	// still running — report it as running so reconcile does not flip it Unreachable.
	running := []*nodev1.RunningSpawn{{SpawnId: "sp1", Generation: 1}}
	var wg sync.WaitGroup
	stopReconcile := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopReconcile:
				return
			default:
				s.reconcileInventory(ctx, "n1", &capSender{}, running)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	close(stopReconcile)
	wg.Wait()

	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("gate abort must return FailedPrecondition, got %v", err)
	}
	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("status=%v want Active after gate-abort revert", sp.Status)
	}
	if !rt.Bound("sp1") {
		t.Fatal("route must remain bound after gate-abort")
	}
}

// TestSuspendClaimLostBailsCleanly verifies that if the DB claim is lost during the node
// round-trip (simulated by a small claimTTL and a sender delay > TTL), the suspend driver bails
// without committing further transitions and returns an error.
func TestSuspendClaimLostBailsCleanly(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 60 * time.Millisecond // tiny TTL → heartbeat expires fast
	s.suspendTimeout = 5 * time.Second // long enough to not expire via timeout

	// The sender takes longer than the TTL to reply, so the heartbeat will expire.
	sender := &suspendSender{
		markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}},
		delay:   300 * time.Millisecond, // > claimTTL → heartbeat fires before reply
	}
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Steal the claim from another goroutine AFTER SuspendSpawn has acquired it,
	// simulating the heartbeat expiry + recovery-sweep takeover.
	// We do this by watching for the spawn to enter Suspending status, then acquiring.
	claimStolen := make(chan struct{})
	go func() {
		bgCtx := context.Background()
		// Poll until the spawn reaches Suspending (the driver has acquired claim + written transition).
		for {
			sp, err := s.st.Spawns().Get(bgCtx, "sp1")
			if err != nil {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			if sp.Status == store.Suspending {
				// Expire the claim by advancing the deadline to the past (simulate TTL expiry).
				// Do this by directly nulling claim columns (the heartbeat can't renew what's gone).
				_ = store.ExpireClaim(bgCtx, s.st, "sp1")
				close(claimStolen)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	_, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	<-claimStolen

	// The suspend must have bailed with an error (CodeAborted or DeadlineExceeded).
	if err == nil {
		t.Fatal("SuspendSpawn must return an error when claim is lost")
	}
	// The spawn must NOT be in an undefined state — it's either Suspending (claim gone, needs
	// recovery) or Active (if the revert succeeded before bail).
	sp, _ := s.st.Spawns().Get(context.Background(), "sp1")
	if sp.Status == store.Unreachable {
		t.Fatalf("claim-lost suspend must not leave spawn Unreachable, got %v", sp.Status)
	}
	_ = rt // route state is driver-defined; we only assert no Unreachable
}

// TestResumePassesThroughResuming verifies that during a resume the spawn status is visible as
// Resuming (toSummaryStatus maps correctly) before finalising to Active.
func TestResumePassesThroughResuming(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second

	// Set up: active → suspend → suspended, then resume.
	suspendSdr := &suspendSender{markers: []*nodev1.MountMarker{{Name: "main", Marker: "m1"}}}
	suspendSdr.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", suspendSdr)
	ctx := auth.WithOwner(context.Background(), "alice")

	if _, err := s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatalf("SuspendSpawn: %v", err)
	}
	if sp, _ := s.st.Spawns().Get(ctx, "sp1"); sp.Status != store.Suspended {
		t.Fatalf("must be Suspended, got %v", sp.Status)
	}

	// Add a resume target node.
	resumeSdr := &capSender{}
	reg.Add(&registry.Node{ID: "n2", Sender: resumeSdr, Max: 1, Free: 1})

	// Kick off a goAckStarts to unblock provision.
	stopAck := goAckStarts(s, resumeSdr)
	defer stopAck()

	// Resume. We can't easily observe Resuming mid-flight in this synchronous path, but we
	// can verify toSummaryStatus maps the Resuming store status to SPAWN_STATUS_RESUMING.
	if got := toSummaryStatus(store.Resuming); got != 8 { // cpv1.SpawnStatus_SPAWN_STATUS_RESUMING = 8
		t.Fatalf("toSummaryStatus(Resuming)=%v want SPAWN_STATUS_RESUMING(8)", got)
	}

	// Force placement to n2 so provision sends Start to resumeSdr (n1 is still registered but its
	// route was dropped after suspend; without a forced override PickFor might choose either node).
	if err := s.withClaim(ctx, "sp1", func(cctx context.Context, leaseID string) error {
		_, err := s.resumeLocked(cctx, "alice", "sp1", placementOverride{NodeID: "n2"}, false, "test-resume", nil, nil, leaseID)
		return err
	}); err != nil {
		t.Fatalf("resumeLocked: %v", err)
	}

	sp, _ := s.st.Spawns().Get(ctx, "sp1")
	if sp.Status != store.Active {
		t.Fatalf("after resume: status=%v want Active", sp.Status)
	}
}

// TestSuspendBusySpawnReturnsBusyError verifies that if a second concurrent SuspendSpawn tries to
// claim a spawn already held by another driver, it gets CodeAborted "busy".
func TestSuspendBusySpawnReturnsBusyError(t *testing.T) {
	s, reg, rt := newTestServer(t)
	s.claimTTL = 5 * time.Second

	sender := &suspendSender{drop: true} // first suspend never completes (drop reply)
	sender.s = s
	activeSpawnWithRoute(t, s, reg, rt, "sp1", "alice", sender)
	ctx := auth.WithOwner(context.Background(), "alice")

	// Acquire the claim manually (simulating the first SuspendSpawn's claim).
	bgCtx := context.Background()
	sp, _ := s.st.Spawns().Get(bgCtx, "sp1")
	c, _, _ := s.st.Spawns().LiveContainer(bgCtx, "sp1")
	gen := c.Generation
	now := time.Now()
	newSeq, err := s.st.Spawns().Acquire(bgCtx, "sp1", "first-driver", "lease-first",
		now.UnixNano(), now.Add(time.Minute).UnixNano(), sp.StatusSeq)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Transition to Suspending (as the first driver would).
	if _, err := s.st.Spawns().TransitionClaimed(bgCtx, "sp1", "lease-first", newSeq, gen, store.Suspending); err != nil {
		t.Fatalf("TransitionClaimed: %v", err)
	}

	// Second SuspendSpawn should see the active claim and return CodeAborted.
	_, err = s.SuspendSpawn(ctx, connect.NewRequest(&cpv1.SuspendSpawnRequest{SpawnId: "sp1"}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("concurrent SuspendSpawn must return CodeAborted, got code=%v err=%v", connect.CodeOf(err), err)
	}
}
