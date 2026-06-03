package node

import (
	"sync"
	"testing"
	"time"
)

// capSender collects frames a client received, thread-safe, with wait/snapshot helpers.
type capSender struct {
	mu  sync.Mutex
	got []Frame
}
func (c *capSender) send(line []byte) error {
	f, _ := decodeFrame(line)
	c.mu.Lock(); c.got = append(c.got, f); c.mu.Unlock()
	return nil
}
func (c *capSender) seqs() []int64 {
	c.mu.Lock(); defer c.mu.Unlock()
	out := make([]int64, len(c.got))
	for i, f := range c.got { out[i] = f.Seq }
	return out
}
// frames returns a race-safe snapshot (tests must NOT iterate c.got directly — the client goroutine
// writes to it concurrently).
func (c *capSender) frames() []Frame {
	c.mu.Lock(); defer c.mu.Unlock()
	return append([]Frame(nil), c.got...)
}
// waitLen polls until the client has received n frames, or fails after 2s.
func (c *capSender) waitLen(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock(); l := len(c.got); c.mu.Unlock()
		if l >= n { return }
		if time.Now().After(deadline) { t.Fatalf("timeout: got %d frames, want %d", l, n) }
		time.Sleep(5 * time.Millisecond)
	}
}

// newTestPump builds a pump with no agent (stdin/stdout nil) for fan-out-only tests.
func newTestPump() *Pump { return newPump(nil, nil) }

func TestFanoutTwoClientsReceiveInOrder(t *testing.T) {
	p := newTestPump()
	a, b := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("b", 0, b.send)
	p.appendFrames([]Frame{{Kind: "agent", Text: "x"}, {Kind: "agent", Text: "y"}})
	a.waitLen(t, 2); b.waitLen(t, 2)
	if got := a.seqs(); got[0] != 1 || got[1] != 2 { t.Fatalf("a seqs %v", got) }
}

func TestLateClientCatchesUpFromCursor(t *testing.T) {
	p := newTestPump()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.appendFrames([]Frame{{Kind: "agent", Text: "1"}, {Kind: "agent", Text: "2"}})
	a.waitLen(t, 2)
	// b joins fresh (cursor 0) -> replays both; c resumes from seq 1 -> gets only seq 2.
	b, c := &capSender{}, &capSender{}
	p.attachClient("b", 0, b.send)
	p.attachClient("c", 1, c.send)
	b.waitLen(t, 2); c.waitLen(t, 1)
	if got := c.seqs(); got[0] != 2 { t.Fatalf("c resume seqs %v, want [2]", got) }
}

func TestDetachOneDoesNotDisturbOthers(t *testing.T) {
	p := newTestPump()
	a, b := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("b", 0, b.send)
	p.detachClient("a")
	p.detachClient("a") // double-detach is a no-op
	p.appendFrames([]Frame{{Kind: "agent", Text: "z"}})
	b.waitLen(t, 1)
}

func TestReconnectOverlapNoLeak(t *testing.T) {
	// Attach a new clientID before detaching the old: both coexist; the old detach removes only itself.
	p := newTestPump()
	a, a2 := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("a2", 0, a2.send) // "reconnect" as a fresh id
	p.appendFrames([]Frame{{Kind: "agent", Text: "1"}})
	a.waitLen(t, 1); a2.waitLen(t, 1)
	p.detachClient("a") // stale detach of the old id
	p.appendFrames([]Frame{{Kind: "agent", Text: "2"}})
	a2.waitLen(t, 2) // a2 still live
}

func TestTrimResetsLaggingClient(t *testing.T) {
	p := newTestPump()
	p.maxLog = 2
	for i := 0; i < 5; i++ { p.appendFrames([]Frame{{Kind: "agent", Text: "x"}}) } // seq 1..5; trims to base=3, log=[seq4,seq5]
	a := &capSender{}
	p.attachClient("a", 1, a.send) // cursor 1 < base 3 -> reset{fromSeq:3} then seq 4,5
	a.waitLen(t, 3)
	fs := a.frames()
	if fs[0].Kind != "reset" || fs[0].FromSeq != 3 { t.Fatalf("want reset{3} first, got %+v", fs[0]) }
	if fs[1].Seq != 4 || fs[2].Seq != 5 { t.Fatalf("want seq 4,5 after reset, got %v", a.seqs()) }
}

// A client whose cursor is exactly at base did NOT miss anything: resume cleanly, no reset.
func TestClientAtBaseResumesWithoutReset(t *testing.T) {
	p := newTestPump()
	p.maxLog = 2
	for i := 0; i < 3; i++ { p.appendFrames([]Frame{{Kind: "agent", Text: "x"}}) } // seq 1..3; base=1, log=[seq2,seq3]
	a := &capSender{}
	p.attachClient("a", 1, a.send) // cursor 1 == base 1 -> NO reset, just seq 2,3
	a.waitLen(t, 2)
	fs := a.frames()
	if fs[0].Kind == "reset" { t.Fatalf("unexpected reset at cursor==base: %+v", fs) }
	if fs[0].Seq != 2 || fs[1].Seq != 3 { t.Fatalf("want seq 2,3, got %v", a.seqs()) }
}
