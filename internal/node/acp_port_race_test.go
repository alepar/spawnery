package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

// TestCloseDuringACPLaunchDoesNotDoubleFreePortAcrossRealloc reproduces the close-during-launch race
// (sp-npxq.3 review): an acp session is closed while its launch goroutine is still mid-handshake, a NEW
// acp session reserves the just-freed (lowest) port in between, and then the stale launch goroutine
// runs its mid-launch undo and frees the port AGAIN. With a bare map-delete freePort the stale undo
// steals the NEW session's reservation (double-free across a realloc); with the ownership-checked
// freePort the stale undo is a no-op and the new session keeps its port.
//
// Determinism: the fake parks session 1's DialACP (before its mid-launch re-check) so the test can
// land CloseSession(1) and a new CreateSession(2) — which reserves the freed port — before releasing
// session 1 to run its undo. reg.onFreePort signals each freePort so the test waits for BOTH the
// close's free and the stale undo's free to complete before asserting (no timing guesswork).
func TestCloseDuringACPLaunchDoesNotDoubleFreePortAcrossRealloc(t *testing.T) {
	ctx := context.Background()
	sx := &fakeSessionExec{
		dialGate:    make(chan struct{}),
		dialReached: make(chan struct{}),
	}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	reg := a.sessions["s1"]

	// Observe every freePort so we can deterministically wait for the stale undo's free.
	freed := make(chan struct{}, 8)
	reg.onFreePort = func() { freed <- struct{}{} }

	// 1) Session 1 (acp): reserves the lowest pool port and parks inside DialACP (before its re-check).
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	<-sx.dialReached // session 1 is now mid-launch, holding port acpPoolLo (owner "1")

	if got := reg.portOwner(acpPoolLo); got != "1" {
		t.Fatalf("precondition: port %d owner = %q, want session \"1\"", acpPoolLo, got)
	}

	// 2) CloseSession(1) lands while session 1 is still STARTING: frees acpPoolLo (owner "1").
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{
		SpawnId: "s1", SessionId: "1",
	}}})
	<-freed // the close's freePort completed; acpPoolLo is now free

	// 3) A NEW acp session (2) reserves the just-freed lowest port acpPoolLo (owner "2") and goes ACTIVE.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	waitFor(t, "session 2 acp ACTIVE", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.pumps[sessionKey{"s1", "2"}]
		return ok
	})
	if got := reg.portOwner(acpPoolLo); got != "2" {
		t.Fatalf("after realloc: port %d owner = %q, want new session \"2\"", acpPoolLo, got)
	}

	// 4) Release session 1: it resumes, fails its mid-launch re-check (closed), and runs its undo —
	//    which frees acpPoolLo a SECOND time, now claiming to be session "1".
	close(sx.dialGate)
	<-freed // the stale undo's freePort completed

	// The stale undo must NOT have stolen session 2's port: acpPoolLo still belongs to "2", and the
	// next allocation does not collide with it. (Pre-fix: the bare delete removes acpPoolLo, so this
	// owner is "" and allocPort below hands acpPoolLo back out, colliding with the live session 2.)
	if got := reg.portOwner(acpPoolLo); got != "2" {
		t.Fatalf("stale undo double-freed across realloc: port %d owner = %q, want \"2\"", acpPoolLo, got)
	}
	p, ok := reg.allocPort("3")
	if !ok {
		t.Fatal("allocPort unexpectedly exhausted")
	}
	if p == acpPoolLo {
		t.Fatalf("port %d handed out again while session 2 still owns it (double-free collision)", acpPoolLo)
	}
}

// TestCloseRacesACPLaunchLeavesNoOrphanPump is the C1 regression (sp-npxq.3 review): a CloseSession
// must not orphan the launch goroutine's just-started Pump when the launch registers AFTER close's a.mu
// pump/relay teardown.
//
// The bug (pre-fix ordering): closeSession published the registry remove LAST — after the a.mu teardown
// AND after the slow KillTmux exec. So a launch that finished its handshake during that window saw the
// entry STILL LIVE, registered its Pump into a.pumps, went ACTIVE — and then close's trailing remove
// dropped the entry, stranding a fully-started Pump in a.pumps that nobody ever .stop()'d (its writeLoop
// blocks forever; its acp connection is never closed; a later allocID can overwrite the slot).
//
// The fix moves reg.remove BEFORE the a.mu teardown, so the launch's mid-launch re-check sees !live and
// undoes its OWN pump (single owner). Determinism: the fake parks session 1's DialACP (so the launch is
// mid-handshake) and gates close INSIDE its teardown window via the FIRST KillTmux; only then is the
// launch released to race. We assert no pump is left in a.pumps for the key and the acp connection was
// closed exactly once (the undo's stop) — i.e. the pump was torn down, not orphaned.
//
// Pre-fix this FAILS: the launch registers the pump (a.pumps[key] present) and never stops it
// (acpClosed == 0). Post-fix it passes: the pump is never registered and is stopped exactly once.
func TestCloseRacesACPLaunchLeavesNoOrphanPump(t *testing.T) {
	ctx := context.Background()
	sx := &fakeSessionExec{
		dialGate:    make(chan struct{}),
		dialReached: make(chan struct{}),
		killGate:    make(chan struct{}),
		killReached: make(chan struct{}),
	}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	key := sessionKey{"s1", "1"}

	// 1) Session 1 (acp) reserves the lowest port and parks inside DialACP (before its mid-launch re-check).
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	<-sx.dialReached // session 1 is mid-launch (entry "1" live, no pump registered yet)

	// 2) CloseSession(1) on its own goroutine: it removes the entry, runs the a.mu teardown (finds no
	//    pump — launch is still parked), then parks INSIDE its first KillTmux (the teardown window).
	closeDone := make(chan struct{})
	go func() {
		a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CloseSession{CloseSession: &nodev1.CloseSession{
			SpawnId: "s1", SessionId: "1",
		}}})
		close(closeDone)
	}()
	<-sx.killReached // close is now inside its teardown window

	// 3) Release the parked launch so it resumes its handshake and reaches the mid-launch re-check WHILE
	//    close is parked. (Pre-fix: re-check sees live -> registers pump. Post-fix: sees !live -> undo.)
	close(sx.dialGate)
	waitFor(t, "launch settled", func() bool {
		a.mu.Lock()
		_, hasPump := a.pumps[key]
		a.mu.Unlock()
		sx.mu.Lock()
		closed := sx.acpClosed
		sx.mu.Unlock()
		return hasPump || closed > 0 // registered (pre-fix) or stopped (post-fix)
	})

	// 4) Let close finish, then assert no orphan.
	close(sx.killGate)
	<-closeDone

	a.mu.Lock()
	_, orphan := a.pumps[key]
	a.mu.Unlock()
	if orphan {
		t.Fatal("orphaned pump left in a.pumps after close-races-launch (launch registered after close teardown)")
	}
	sx.mu.Lock()
	closed := sx.acpClosed
	sx.mu.Unlock()
	if closed != 1 {
		t.Fatalf("acp connection closed %d times, want exactly 1 (pump must be stopped exactly once, not orphaned)", closed)
	}
}
