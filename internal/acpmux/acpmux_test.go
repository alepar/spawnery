package acpmux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spawnery/internal/acp"
)

// fakeAgent is an in-process ACP agent wired to acpmux's upstream pipes. It
// answers initialize / session/new, streams session/update chunks on a prompt
// then replies (turn end), and can emit a session/request_permission on demand.
// It records the order of upstream events so tests can assert serialization and
// first-wins permission resolution.
type fakeAgent struct {
	in  *acp.Reader // messages FROM acpmux (acpmux's stdin)
	out io.Writer   // messages TO acpmux (acpmux's stdout)

	mu        sync.Mutex
	inflight  int32 // number of prompts acpmux currently has in flight upstream
	maxInflt  int32 // high-water mark
	permReplies [][]byte // permission responses received from acpmux (should be exactly 1)

	// gate, when non-nil, blocks each prompt's turn until released, so the test
	// can verify only one prompt is in flight at a time.
	promptGate chan struct{}

	wmu sync.Mutex
}

func newFakeAgent(toMux io.Writer, fromMux io.Reader) *fakeAgent {
	return &fakeAgent{in: acp.NewReader(fromMux), out: toMux}
}

func (f *fakeAgent) write(m acp.Message) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = acp.WriteMessage(f.out, m)
}

func (f *fakeAgent) run() {
	for {
		msg, err := f.in.ReadMessage()
		if err != nil {
			return
		}
		switch {
		case msg.Method == "initialize":
			f.write(acp.Message{ID: msg.ID, Result: json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"authMethods":[]}`)})
		case msg.Method == "session/new":
			f.write(acp.Message{ID: msg.ID, Result: json.RawMessage(`{"sessionId":"S1"}`)})
		case msg.Method == "session/prompt":
			go f.handlePrompt(msg)
		case msg.ID != nil && msg.Method == "" && msg.Result != nil:
			// a response to OUR session/request_permission (the perm answer routed
			// from acpmux). Record it; there must be exactly one.
			f.mu.Lock()
			f.permReplies = append(f.permReplies, append([]byte(nil), msg.Result...))
			f.mu.Unlock()
		}
	}
}

func (f *fakeAgent) handlePrompt(msg acp.Message) {
	cur := atomic.AddInt32(&f.inflight, 1)
	f.mu.Lock()
	if cur > f.maxInflt {
		f.maxInflt = cur
	}
	gate := f.promptGate
	f.mu.Unlock()

	text := promptText(msg.Params)
	// Stream two agent_message_chunks for this turn.
	for i := 0; i < 2; i++ {
		upd, _ := json.Marshal(map[string]any{
			"sessionId": "S1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": fmt.Sprintf("[%s#%d]", text, i)},
			},
		})
		f.write(acp.Message{Method: "session/update", Params: upd})
	}

	if gate != nil {
		<-gate // block the turn until the test releases it
	}

	atomic.AddInt32(&f.inflight, -1)
	f.write(acp.Message{ID: msg.ID, Result: json.RawMessage(`{"stopReason":"end_turn"}`)})
}

// emitPermission sends a server-initiated session/request_permission with the
// given upstream id.
func (f *fakeAgent) emitPermission(id int) {
	params, _ := json.Marshal(map[string]any{
		"sessionId": "S1",
		"toolCall":  map[string]any{"title": "run rm -rf"},
		"options": []map[string]any{
			{"optionId": "allow-once", "kind": "allow_once"},
			{"optionId": "reject-once", "kind": "reject_once"},
		},
	})
	f.write(acp.Message{ID: &id, Method: "session/request_permission", Params: params})
}

// ---- downstream test client -------------------------------------------------

// testClient is a raw ACP client connected to acpmux over TCP.
type testClient struct {
	conn net.Conn
	rd   *acp.Reader
	nid  int
}

func dial(t *testing.T, addr string) *testClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return &testClient{conn: conn, rd: acp.NewReader(conn)}
}

func (tc *testClient) send(m acp.Message) { _ = acp.WriteMessage(tc.conn, m) }

func (tc *testClient) call(t *testing.T, method string, params json.RawMessage) acp.Message {
	t.Helper()
	tc.nid++
	id := tc.nid
	tc.send(acp.Message{ID: &id, Method: method, Params: params})
	for {
		m := tc.read(t)
		if m.ID != nil && *m.ID == id && (m.Result != nil || m.Error != nil) {
			return m
		}
	}
}

func (tc *testClient) sendPrompt(t *testing.T, text string) int {
	t.Helper()
	tc.nid++
	id := tc.nid
	p, _ := json.Marshal(map[string]any{
		"sessionId": "S1",
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	})
	tc.send(acp.Message{ID: &id, Method: "session/prompt", Params: p})
	return id
}

func (tc *testClient) read(t *testing.T) acp.Message {
	t.Helper()
	_ = tc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := tc.rd.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	return m
}

func (tc *testClient) handshake(t *testing.T) {
	t.Helper()
	tc.call(t, "initialize", json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`))
	tc.call(t, "session/new", json.RawMessage(`{"cwd":"/app","mcpServers":[]}`))
}

func (tc *testClient) close() { _ = tc.conn.Close() }

// updateText extracts the agent_message_chunk text from a session/update, "" if not one.
func updateText(m acp.Message) string {
	if m.Method != "session/update" {
		return ""
	}
	var u struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(m.Params, &u) != nil || u.Update.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	return u.Update.Content.Text
}

// ---- harness ----------------------------------------------------------------

// startMux wires a fakeAgent to a new Mux over io.Pipe pairs, starts it, and
// listens on a loopback TCP port. Returns the listen addr, the fake agent, and
// a teardown.
func startMux(t *testing.T) (addr string, fa *fakeAgent, teardown func()) {
	t.Helper()
	// acpmux writes to its "stdin" -> the agent reads it; agent writes to its out
	// -> acpmux reads it as "stdout".
	muxStdinR, muxStdinW := io.Pipe() // mux -> agent
	agentOutR, agentOutW := io.Pipe() // agent -> mux

	m := New(muxStdinW, agentOutR)
	fa = newFakeAgent(agentOutW, muxStdinR)
	go fa.run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx, 3*time.Second); err != nil {
		t.Fatalf("mux start: %v", err)
	}
	if m.SessionID() != "S1" {
		t.Fatalf("session id = %q, want S1", m.SessionID())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = m.Serve(ln) }()

	teardown = func() {
		_ = ln.Close()
		m.Stop()
		_ = muxStdinW.Close()
		_ = agentOutW.Close()
	}
	return ln.Addr().String(), fa, teardown
}

// collectUpdates reads from the client until it has seen wantN agent_message_chunk
// texts (or a deadline). Returns the collected texts.
func collectUpdates(t *testing.T, tc *testClient, wantN int) []string {
	t.Helper()
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < wantN && time.Now().Before(deadline) {
		_ = tc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		m, err := tc.rd.ReadMessage()
		if err != nil {
			t.Fatalf("collectUpdates read: %v (got %d/%d: %v)", err, len(got), wantN, got)
		}
		if txt := updateText(m); txt != "" {
			got = append(got, txt)
		}
	}
	if len(got) < wantN {
		t.Fatalf("got %d updates, want %d: %v", len(got), wantN, got)
	}
	return got
}

// TestFanout: client A prompts; BOTH A and B receive the same session/update notifications.
func TestFanout(t *testing.T) {
	addr, _, teardown := startMux(t)
	defer teardown()

	a := dial(t, addr)
	defer a.close()
	b := dial(t, addr)
	defer b.close()
	a.handshake(t)
	b.handshake(t)

	reqID := a.sendPrompt(t, "hello")

	// Both clients must receive the two streamed chunks.
	gotA := collectUpdates(t, a, 2)
	gotB := collectUpdates(t, b, 2)
	want := []string{"[hello#0]", "[hello#1]"}
	for i := range want {
		if gotA[i] != want[i] {
			t.Fatalf("A update %d = %q, want %q", i, gotA[i], want[i])
		}
		if gotB[i] != want[i] {
			t.Fatalf("B update %d = %q, want %q", i, gotB[i], want[i])
		}
	}

	// A (the prompter) must also receive its session/prompt result (turn end).
	for {
		m := a.read(t)
		if m.ID != nil && *m.ID == reqID && m.Result != nil {
			break
		}
	}
}

// TestLateJoinReplay: A prompts and gets updates; THEN B connects + session/new's
// and receives the replayed history.
func TestLateJoinReplay(t *testing.T) {
	addr, _, teardown := startMux(t)
	defer teardown()

	a := dial(t, addr)
	defer a.close()
	a.handshake(t)

	reqID := a.sendPrompt(t, "early")
	collectUpdates(t, a, 2)
	// Drain A's turn-end so the turn is fully complete before B joins.
	for {
		m := a.read(t)
		if m.ID != nil && *m.ID == reqID && m.Result != nil {
			break
		}
	}

	// Late joiner.
	b := dial(t, addr)
	defer b.close()
	b.handshake(t) // session/new triggers replay

	gotB := collectUpdates(t, b, 2)
	want := []string{"[early#0]", "[early#1]"}
	for i := range want {
		if gotB[i] != want[i] {
			t.Fatalf("late-join B replay %d = %q, want %q", i, gotB[i], want[i])
		}
	}
}

// TestSerializedPrompts: A prompts and B prompts while A's turn is in flight;
// only one upstream session/prompt is in flight at a time and both turns complete.
func TestSerializedPrompts(t *testing.T) {
	addr, fa, teardown := startMux(t)
	defer teardown()

	gate := make(chan struct{})
	fa.mu.Lock()
	fa.promptGate = gate
	fa.mu.Unlock()

	a := dial(t, addr)
	defer a.close()
	b := dial(t, addr)
	defer b.close()
	a.handshake(t)
	b.handshake(t)

	aReq := a.sendPrompt(t, "AAA")
	// Wait until A's prompt is in flight upstream.
	waitInflight(t, fa, 1)
	bReq := b.sendPrompt(t, "BBB") // queued (A in flight)

	// Give acpmux a moment; B must NOT cause a second in-flight prompt.
	time.Sleep(200 * time.Millisecond)
	fa.mu.Lock()
	maxAfter := fa.maxInflt
	fa.mu.Unlock()
	if maxAfter != 1 {
		t.Fatalf("max in-flight upstream prompts = %d, want 1 (prompts not serialized)", maxAfter)
	}

	// Release A's turn; then B's turn should start and also complete.
	gate <- struct{}{} // end A's turn
	// A's result.
	awaitResult(t, a, aReq)
	gate <- struct{}{} // end B's turn (drained as next)
	awaitResult(t, b, bReq)

	fa.mu.Lock()
	maxFinal := fa.maxInflt
	fa.mu.Unlock()
	if maxFinal != 1 {
		t.Fatalf("max in-flight = %d after both turns, want 1", maxFinal)
	}
}

// TestPermissionBroadcastFirstWins: the fake agent emits session/request_permission;
// BOTH clients receive it; the FIRST to respond resolves it; exactly one outcome
// reaches the agent; the second response no-ops.
func TestPermissionBroadcastFirstWins(t *testing.T) {
	addr, fa, teardown := startMux(t)
	defer teardown()

	a := dial(t, addr)
	defer a.close()
	b := dial(t, addr)
	defer b.close()
	a.handshake(t)
	b.handshake(t)

	const permID = 4242
	fa.emitPermission(permID)

	// Both clients must receive a session/request_permission with id permID.
	permA := awaitPermRequest(t, a, permID)
	permB := awaitPermRequest(t, b, permID)
	if permA.ID == nil || *permA.ID != permID || permB.ID == nil || *permB.ID != permID {
		t.Fatalf("perm request ids: A=%v B=%v want %d", permA.ID, permB.ID, permID)
	}

	// A answers allow first.
	idv := permID
	allowResp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})
	a.send(acp.Message{ID: &idv, Result: allowResp})

	// B answers later (deny) — must no-op.
	time.Sleep(150 * time.Millisecond)
	idv2 := permID
	denyResp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject-once"}})
	b.send(acp.Message{ID: &idv2, Result: denyResp})

	// Wait for the agent to record exactly one perm reply, and verify it's the allow.
	deadline := time.Now().Add(3 * time.Second)
	for {
		fa.mu.Lock()
		n := len(fa.permReplies)
		fa.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never received a permission reply")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Settle, then assert exactly one and it selected the allow option.
	time.Sleep(200 * time.Millisecond)
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.permReplies) != 1 {
		t.Fatalf("agent received %d perm replies, want exactly 1 (first-wins broken)", len(fa.permReplies))
	}
	var got struct {
		Outcome struct {
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(fa.permReplies[0], &got); err != nil {
		t.Fatalf("bad perm reply: %v", err)
	}
	if got.Outcome.OptionID != "allow-once" {
		t.Fatalf("agent perm reply optionId = %q, want allow-once (first answer should win)", got.Outcome.OptionID)
	}
}

// ---- test helpers -----------------------------------------------------------

func waitInflight(t *testing.T, fa *fakeAgent, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fa.inflight) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upstream in-flight did not reach %d", want)
}

func awaitResult(t *testing.T, tc *testClient, reqID int) {
	t.Helper()
	for {
		m := tc.read(t)
		if m.ID != nil && *m.ID == reqID && m.Result != nil {
			return
		}
	}
}

func awaitPermRequest(t *testing.T, tc *testClient, permID int) acp.Message {
	t.Helper()
	for {
		m := tc.read(t)
		if m.Method == "session/request_permission" {
			return m
		}
	}
}
