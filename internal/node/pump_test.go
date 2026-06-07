package node

import (
	"context"
	"fmt"
	"io"
	"spawnery/internal/acp"
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

func TestConcurrentAppendAndAttachRace(t *testing.T) {
	p := newTestPump()
	const appenders, perAppender = 3, 1000
	var wg sync.WaitGroup
	wg.Add(appenders)
	for w := 0; w < appenders; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perAppender; i++ { p.appendFrames([]Frame{{Kind: "agent", Text: "x"}}) }
		}()
	}
	// clients attaching concurrently with the append storm
	for i := 0; i < 5; i++ {
		c := &capSender{}
		p.attachClient(fmt.Sprintf("c%d", i), 0, c.send)
	}
	wg.Wait()
	total := int64(appenders * perAppender)
	// a client attached AFTER the storm must drain to the final seq.
	late := &capSender{}
	p.attachClient("late", 0, late.send)
	deadline := time.Now().Add(3 * time.Second)
	for {
		fr := late.frames()
		if n := len(fr); n > 0 && fr[n-1].Seq == total { break }
		if time.Now().After(deadline) {
			var last int64
			if fr := late.frames(); len(fr) > 0 { last = fr[len(fr)-1].Seq }
			t.Fatalf("late client reached seq %d, want %d", last, total)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A cursor ahead of the pump's seq (e.g. pump restarted) is treated as out-of-range -> reset.
func TestFutureCursorResets(t *testing.T) {
	p := newTestPump()
	p.appendFrames([]Frame{{Kind: "agent", Text: "x"}}) // seq 1
	a := &capSender{}
	p.attachClient("a", 100, a.send) // cursor 100 > seq 1 -> reset{fromSeq:0} then seq 1
	a.waitLen(t, 2)
	fs := a.frames()
	if fs[0].Kind != "reset" || fs[0].FromSeq != 0 { t.Fatalf("want reset{0} first, got %+v", fs[0]) }
	if fs[1].Seq != 1 { t.Fatalf("want seq 1 after reset, got %v", a.seqs()) }
}

// scriptGoose is a fake agent over pipes: answers initialize + session/new, and for each
// session/prompt streams one agent_message_chunk then a result (turn-end).
func scriptGoose(in io.Reader, out io.Writer) {
	rd := acp.NewReader(in)
	for {
		m, err := rd.ReadMessage()
		if err != nil { return }
		switch m.Method {
		case "initialize":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"protocolVersion":1}`)})
		case "session/new":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"sessionId":"s1"}`)})
		case "session/prompt":
			acp.WriteMessage(out, acp.Message{Method: "session/update", Params: []byte(`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ECHO"}}}`)})
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"stopReason":"end_turn"}`)})
		}
	}
}

func TestPromptStreamsAgentFrameAndTurn(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()  // pump.stdin -> gooseInW; goose reads gooseInR
	gooseOutR, gooseOutW := io.Pipe() // goose writes gooseOutW; pump reads gooseOutR
	go scriptGoose(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.stop()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "hi"}))
	a.waitLen(t, 4) // user, turn(busy), agent "ECHO", turn(idle)
	kinds := map[string]int{}
	var sawBusy, sawIdle bool
	for _, f := range a.frames() {
		kinds[f.Kind]++
		if f.Kind == "turn" && f.State == "busy" { sawBusy = true }
		if f.Kind == "turn" && f.State == "idle" { sawIdle = true }
	}
	if kinds["user"] == 0 || kinds["agent"] == 0 { t.Fatalf("missing user/agent frames: %v", kinds) }
	if !sawBusy || !sawIdle { t.Fatalf("want busy and idle turn frames: %v", a.frames()) }
}

func TestStartTimesOutIfAgentSilent(t *testing.T) {
	gooseOutR, _ := io.Pipe() // stdout that never produces -> initialize never answered
	p := newPump(io.Discard, gooseOutR)
	if err := p.start(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("want timeout error")
	}
}

// A prompt sent while a turn is in flight is queued (a user frame is still logged) and drained on
// turn-end.
func TestQueuedPromptDrainsOnTurnEnd(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "one"}))
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "two"})) // may queue if "one" still busy
	// Eventually two user frames (one, two) and two agent ECHO frames appear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		users, agents := 0, 0
		for _, f := range a.frames() { if f.Kind == "user" { users++ }; if f.Kind == "agent" { agents++ } }
		if users >= 2 && agents >= 2 { break }
		if time.Now().After(deadline) { t.Fatalf("want 2 user + 2 agent frames, got %v", a.frames()) }
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopUnblocksReadLoop(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	p.stop()
	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit after stop()")
	}
	p.stop() // idempotent, must not panic
}

func TestStartCancelledByContext(t *testing.T) {
	gooseOutR, _ := io.Pipe() // never answers
	p := newPump(io.Discard, gooseOutR)
	defer p.stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.start(ctx, 10*time.Second); err == nil { // long timeout; ctx cancel must abort fast
		t.Fatal("want error from cancelled ctx")
	}
}

// scriptGoosePerm: for each session/prompt it asks permission (request id 99) instead of finishing,
// and only emits the prompt's end_turn result after it receives the pump's permission response (id 99).
func scriptGoosePerm(in io.Reader, out io.Writer) {
	rd := acp.NewReader(in)
	var promptID *acp.RawID
	for {
		m, err := rd.ReadMessage()
		if err != nil { return }
		switch {
		case m.Method == "initialize":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"protocolVersion":1}`)})
		case m.Method == "session/new":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"sessionId":"s1"}`)})
		case m.Method == "session/prompt":
			promptID = m.ID
			acp.WriteMessage(out, acp.Message{ID: acp.IntID(99), Method: "session/request_permission", Params: []byte(`{"options":[{"optionId":"allow","kind":"allow"},{"optionId":"reject","kind":"reject"}]}`)})
		case idIs99(m) && m.Result != nil:
			if promptID != nil {
				acp.WriteMessage(out, acp.Message{ID: promptID, Result: []byte(`{"stopReason":"end_turn"}`)})
				promptID = nil
			}
		}
	}
}

func startPermPump(t *testing.T) *Pump {
	t.Helper()
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoosePerm(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	return p
}

func waitKind(t *testing.T, c *capSender, kind string) Frame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, f := range c.frames() { if f.Kind == kind { return f } }
		if time.Now().After(deadline) { t.Fatalf("no %q frame; got %v", kind, c.frames()) }
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPermissionBroadcastFirstWins(t *testing.T) {
	p := startPermPump(t); defer p.stop()
	a, b := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("b", 0, b.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "need-perm"}))
	pa := waitKind(t, a, "perm_request")
	waitKind(t, b, "perm_request")
	if pa.Seq != 0 { t.Fatalf("perm_request must be transient (seq 0), got %d", pa.Seq) }
	// b answers first; a's later answer is a no-op.
	p.fromClient("b", encodeFrame(Frame{Kind: "perm_response", ReqID: pa.ReqID, Allow: true}))
	p.fromClient("a", encodeFrame(Frame{Kind: "perm_response", ReqID: pa.ReqID, Allow: false}))
	// turn completes -> both clients see an idle turn frame.
	waitTurnIdle := func(c *capSender) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			for _, f := range c.frames() { if f.Kind == "turn" && f.State == "idle" { return } }
			if time.Now().After(deadline) { t.Fatalf("no idle turn; got %v", c.frames()) }
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitTurnIdle(a); waitTurnIdle(b)
}

func TestPermissionResentOnAttachWhilePending(t *testing.T) {
	p := startPermPump(t); defer p.stop()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "need-perm"}))
	waitKind(t, a, "perm_request") // a got it
	b := &capSender{}
	p.attachClient("b", 0, b.send) // late client must also get the still-pending perm_request
	waitKind(t, b, "perm_request")
}

// scriptGoosePermTitled asks permission with a human-readable toolCall.title (no end_turn here; the
// test only inspects the perm_request frame).
func scriptGoosePermTitled(in io.Reader, out io.Writer) {
	rd := acp.NewReader(in)
	for {
		m, err := rd.ReadMessage()
		if err != nil {
			return
		}
		switch {
		case m.Method == "initialize":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"protocolVersion":1}`)})
		case m.Method == "session/new":
			acp.WriteMessage(out, acp.Message{ID: m.ID, Result: []byte(`{"sessionId":"s1"}`)})
		case m.Method == "session/prompt":
			acp.WriteMessage(out, acp.Message{ID: acp.IntID(99), Method: "session/request_permission",
				Params: []byte(`{"toolCall":{"title":"Run shell: rm -rf /tmp/x"},"options":[{"optionId":"allow","kind":"allow"},{"optionId":"reject","kind":"reject"}]}`)})
		}
	}
}

// idIs99 reports whether m's JSON-RPC id is the integer 99 (the fake agent's
// permission-request id), using the RawID int helper.
func idIs99(m acp.Message) bool { n, ok := m.ID.AsInt(); return ok && n == 99 }

// The pump must surface goose's real toolCall.title on the perm_request frame — on the broadcast AND
// on the re-send to a late-attaching client — not a generic placeholder.
func TestPermissionTitleFromToolCall(t *testing.T) {
	const want = "Run shell: rm -rf /tmp/x"
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoosePermTitled(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	defer p.stop()

	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "do-it"}))
	if pa := waitKind(t, a, "perm_request"); pa.Title != want {
		t.Fatalf("broadcast perm_request title = %q, want %q", pa.Title, want)
	}
	// A late client must get the SAME real title on re-send.
	b := &capSender{}
	p.attachClient("b", 0, b.send)
	if pb := waitKind(t, b, "perm_request"); pb.Title != want {
		t.Fatalf("re-sent perm_request title = %q, want %q", pb.Title, want)
	}
}

// A turn started with NO clients attached must still complete and land in the log; a client that
// attaches afterward replays the whole turn. (The pump drives the agent regardless of clients.)
func TestTurnCompletesWithNoClientsThenReplays(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	// No clients attached. fromClient processes the prompt + logs frames regardless.
	p.fromClient("ghost", encodeFrame(Frame{Kind: "prompt", Text: "hi"}))
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.log)
		idle := false
		for _, f := range p.log { if f.Kind == "turn" && f.State == "idle" { idle = true } }
		p.mu.Unlock()
		if idle && n >= 4 { break } // user, turn busy, agent ECHO, turn idle
		if time.Now().After(deadline) { t.Fatal("turn did not complete into the log with zero clients") }
		time.Sleep(5 * time.Millisecond)
	}
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	a.waitLen(t, 4) // late client replays the whole turn from the log
}

// Two clients fan out; after detaching one, the remaining client still gets new turns.
func TestMultiClientDetachOneStillServesTurns(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	a, b := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("b", 0, b.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "one"}))
	// both clients see the first turn's agent frame
	agentCount := func(c *capSender) int { n := 0; for _, f := range c.frames() { if f.Kind == "agent" { n++ } }; return n }
	waitAgents := func(c *capSender, want int) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if agentCount(c) >= want { return }
			if time.Now().After(deadline) { t.Fatalf("want %d agent frames, got %d", want, agentCount(c)) }
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitAgents(a, 1); waitAgents(b, 1)
	p.detachClient("a")
	p.fromClient("b", encodeFrame(Frame{Kind: "prompt", Text: "two"}))
	waitAgents(b, 2) // b gets the second turn
	if agentCount(a) != 1 { t.Fatalf("detached client a should not get new frames, got %d", agentCount(a)) }
}

func TestPermissionTimeoutDenies(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoosePerm(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	p.permTimeout = 50 * time.Millisecond
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "need-perm"}))
	waitKind(t, a, "perm_request")
	// nobody answers -> auto-deny after 50ms -> goose finishes the turn -> idle turn frame.
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, f := range a.frames() { if f.Kind == "turn" && f.State == "idle" { return } }
		if time.Now().After(deadline) { t.Fatalf("timeout-deny did not complete the turn; got %v", a.frames()) }
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopCallsCloseFnAndExitFnNotOnStop(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	var closed, exited int
	p := newPump(gooseInW, gooseOutR)
	p.closeFn = func() error { closed++; return gooseOutR.Close() } // close stdout so readLoop unblocks
	p.exitFn = func() { exited++ }
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	p.stop()
	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit")
	}
	if closed != 1 { t.Fatalf("closeFn called %d times, want 1", closed) }
	if exited != 0 { t.Fatalf("exitFn must NOT fire on intentional stop, got %d", exited) }
}

func TestExitFnFiresOnAgentDeath(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoose(gooseInR, gooseOutW)
	exited := make(chan struct{}, 1)
	p := newPump(gooseInW, gooseOutR)
	p.exitFn = func() { exited <- struct{}{} }
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	gooseOutW.Close() // agent "dies": stdout EOFs -> readLoop exits -> exitFn fires (not stopped)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exitFn did not fire on agent death")
	}
}
