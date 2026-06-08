package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
)

// framesForClient counts the relay Frames the attacher sent to the CP addressed to clientID (i.e. the
// frames a bound client's send-closure delivered). Zero means the client was never subscribed.
func framesForClient(f *fakeCPStream, clientID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.sent {
		if fr := m.GetFrame(); fr != nil && fr.ClientId == clientID {
			n++
		}
	}
	return n
}

// TestAttachBeforePumpReadyBindsWhenPumpReadies is the multi-session attach-race regression: a client
// (the web) attaches to an additional acp session WHILE that session is still STARTING — its Pump is not
// yet registered (the launch goroutine is mid-handshake). Pre-fix attachClient logged "no pump" and
// DROPPED the attach with no pending state, so when the launch finished it registered a Pump with ZERO
// subscribers — the user saw nothing until a reload re-attached to the now-ready Pump. The fix PENDS the
// attach (the session EXISTS in the registry) and BINDS it when launchACPSession registers the Pump.
//
// Determinism: the fake parks session 1's DialACP (before the Pump is created/registered) so the test
// lands the attach while STARTING, asserts it pended (not dropped), then releases the launch. After the
// Pump readies we drive a prompt and assert the bound client receives the broadcast frames.
//
// Pre-fix this FAILS: the attach is dropped, the client is never a subscriber, framesForClient stays 0
// (the waitFor below times out). Post-fix it passes: the pended attach binds and the client gets frames.
func TestAttachBeforePumpReadyBindsWhenPumpReadies(t *testing.T) {
	ctx := context.Background()
	sx := &fakeSessionExec{
		dialGate:    make(chan struct{}),
		dialReached: make(chan struct{}),
	}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)
	key := sessionKey{"s1", "1"}

	// 1) Session 1 (acp): reserves a pool port and parks inside DialACP — the entry is live (STARTING) but
	//    no Pump is registered yet.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_CreateSession{CreateSession: &nodev1.CreateSession{
		SpawnId: "s1", Transport: nodev1.SessionTransport_SESSION_TRANSPORT_ACP, Runnable: "goose-acp",
	}}})
	<-sx.dialReached // session 1 is mid-launch (entry "1" live, no pump registered yet)

	// 2) The web attaches WHILE session 1 is STARTING. This must PEND (not drop): the session exists.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Open{Open: &nodev1.SessionOpen{
		SpawnId: "s1", SessionId: "1", ClientId: "web-A", Cursor: 0,
	}}})
	a.mu.Lock()
	pendN := len(a.pending[key])
	_, hasPump := a.pumps[key]
	a.mu.Unlock()
	if hasPump {
		t.Fatal("precondition: pump must not be registered yet (launch is parked)")
	}
	if pendN != 1 {
		t.Fatalf("attach during STARTING must PEND, got %d pending (pre-fix drops it -> 0)", pendN)
	}

	// 3) Release the launch: it resumes, registers the Pump, and drains pending -> binds web-A.
	close(sx.dialGate)
	waitFor(t, "pump ready and pended client bound", func() bool {
		a.mu.Lock()
		p := a.pumps[key]
		a.mu.Unlock()
		return p != nil && p.attached() // web-A was bound as a subscriber
	})

	// 4) Drive a prompt; the bound client must receive the broadcast frames (>=1).
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Frame{Frame: &nodev1.Frame{
		SpawnId: "s1", SessionId: "1", ClientId: "web-A", Data: encodeFrame(Frame{Kind: "prompt", Text: "hi"}),
	}}})
	waitFor(t, "bound client receives replayed/broadcast frames", func() bool {
		return framesForClient(fs, "web-A") >= 1
	})

	// pending must be cleared once bound.
	a.mu.Lock()
	leftover := len(a.pending[key])
	a.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("pending not cleared after bind: %d left", leftover)
	}
}

// TestAttachUnknownSessionDoesNotPend keeps the no-pend invariant for a genuinely-unknown session: an
// attach to a session id that is NOT in the registry must NOT be queued (that would queue forever — no
// launch will ever drain it). It no-ops/warns, exactly as before.
func TestAttachUnknownSessionDoesNotPend(t *testing.T) {
	sx := &fakeSessionExec{}
	fs := &fakeCPStream{}
	a := newSessionAttacher("s1", sx, fs)

	a.attachClient("s1", "99", "web-X", 0) // session 99 was never created

	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.pending[sessionKey{"s1", "99"}]); n != 0 {
		t.Fatalf("attach to unknown session must NOT pend, got %d pending", n)
	}
	if _, ok := a.pumps[sessionKey{"s1", "99"}]; ok {
		t.Fatal("attach to unknown session must not create a pump")
	}
}
