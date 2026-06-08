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

	if got := reg.ports[acpPoolLo]; got != "1" {
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
	if got := reg.ports[acpPoolLo]; got != "2" {
		t.Fatalf("after realloc: port %d owner = %q, want new session \"2\"", acpPoolLo, got)
	}

	// 4) Release session 1: it resumes, fails its mid-launch re-check (closed), and runs its undo —
	//    which frees acpPoolLo a SECOND time, now claiming to be session "1".
	close(sx.dialGate)
	<-freed // the stale undo's freePort completed

	// The stale undo must NOT have stolen session 2's port: acpPoolLo still belongs to "2", and the
	// next allocation does not collide with it. (Pre-fix: the bare delete removes acpPoolLo, so this
	// owner is "" and allocPort below hands acpPoolLo back out, colliding with the live session 2.)
	if got := reg.ports[acpPoolLo]; got != "2" {
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
