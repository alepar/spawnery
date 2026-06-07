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
	setModes  []string // modeId of each session/set_mode received from acpmux (cat F)
	cancels   []string // sessionId of each session/cancel notification received from acpmux (cat J)

	// modesJSON, when non-empty, is the SessionModeState advertised on the session/new result (cat F).
	modesJSON string

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
			res := `{"sessionId":"S1"}`
			f.mu.Lock()
			mj := f.modesJSON
			f.mu.Unlock()
			if mj != "" {
				res = `{"sessionId":"S1","modes":` + mj + `}`
			}
			f.write(acp.Message{ID: msg.ID, Result: json.RawMessage(res)})
		case msg.Method == "session/set_mode":
			// Record the switch and announce it via a current_mode_update notification (fans out downstream).
			var p struct {
				ModeID string `json:"modeId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			f.setModes = append(f.setModes, p.ModeID)
			f.mu.Unlock()
			if msg.ID != nil {
				f.write(acp.Message{ID: msg.ID, Result: json.RawMessage(`{}`)})
			}
			upd, _ := json.Marshal(map[string]any{
				"sessionId": "S1",
				"update":    map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": p.ModeID},
			})
			f.write(acp.Message{Method: "session/update", Params: upd})
		case msg.Method == "session/cancel":
			// A turn interrupt forwarded from any downstream client (cat J). It is a notification (no id).
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			f.cancels = append(f.cancels, p.SessionID)
			f.mu.Unlock()
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
	f.write(acp.Message{ID: acp.IntID(id), Method: "session/request_permission", Params: params})
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
	tc.send(acp.Message{ID: acp.IntID(id), Method: method, Params: params})
	for {
		m := tc.read(t)
		if n, ok := m.ID.AsInt(); ok && n == id && (m.Result != nil || m.Error != nil) {
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
	tc.send(acp.Message{ID: acp.IntID(id), Method: "session/prompt", Params: p})
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
		if n, ok := m.ID.AsInt(); ok && n == reqID && m.Result != nil {
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
		if n, ok := m.ID.AsInt(); ok && n == reqID && m.Result != nil {
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
	if na, oka := permA.ID.AsInt(); !oka || na != permID {
		t.Fatalf("perm request id A=%v want %d", permA.ID, permID)
	}
	if nb, okb := permB.ID.AsInt(); !okb || nb != permID {
		t.Fatalf("perm request id B=%v want %d", permB.ID, permID)
	}

	// A answers allow first.
	allowResp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"}})
	a.send(acp.Message{ID: acp.IntID(permID), Result: allowResp})

	// B answers later (deny) — must no-op.
	time.Sleep(150 * time.Millisecond)
	denyResp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "reject-once"}})
	b.send(acp.Message{ID: acp.IntID(permID), Result: denyResp})

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
		if n, ok := m.ID.AsInt(); ok && n == reqID && m.Result != nil {
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

// startMuxWithModes is startMux with the upstream advertising the given SessionModeState JSON on
// session/new (cat F). Set before Start so the cached modes are populated.
func startMuxWithModes(t *testing.T, modesJSON string) (addr string, fa *fakeAgent, teardown func()) {
	t.Helper()
	muxStdinR, muxStdinW := io.Pipe()
	agentOutR, agentOutW := io.Pipe()
	m := New(muxStdinW, agentOutR)
	fa = newFakeAgent(agentOutW, muxStdinR)
	fa.modesJSON = modesJSON
	go fa.run()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx, 3*time.Second); err != nil {
		t.Fatalf("mux start: %v", err)
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

func (f *fakeAgent) setModeIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setModes...)
}

func (f *fakeAgent) cancelSessionIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

// A downstream session/cancel from ANY client must forward upstream to interrupt the shared turn
// (cat J, sp-ufz.13) — the former v1 no-op is now real. v1 shared-attach: any client may cancel (no
// arbitration). The forwarded message is a notification carrying the shared session id.
func TestCancelForwardsUpstream(t *testing.T) {
	addr, fa, teardown := startMux(t)
	defer teardown()
	a, b := dial(t, addr), dial(t, addr)
	defer a.close()
	defer b.close()
	a.handshake(t)
	b.handshake(t)

	// Client B cancels the shared session's active turn.
	b.send(acp.Message{Method: "session/cancel", Params: json.RawMessage(`{"sessionId":"S1"}`)})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if cs := fa.cancelSessionIDs(); len(cs) == 1 {
			if cs[0] != "S1" {
				t.Fatalf("upstream cancel sessionId = %q, want S1", cs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream never received the session/cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The downstream session/new response must re-advertise the upstream's session modes so each client
// can render a mode selector for the shared session (cat F).
func TestSessionNewReAdvertisesModes(t *testing.T) {
	addr, _, teardown := startMuxWithModes(t, `{"currentModeId":"build","availableModes":[{"id":"build","name":"Build"},{"id":"plan","name":"Plan"}]}`)
	defer teardown()
	tc := dial(t, addr)
	defer tc.close()
	tc.call(t, "initialize", json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`))
	res := tc.call(t, "session/new", json.RawMessage(`{"cwd":"/app","mcpServers":[]}`))
	s := string(res.Result)
	for _, want := range []string{`"currentModeId":"build"`, `"availableModes"`, `"id":"build"`, `"id":"plan"`} {
		if !contains(s, want) {
			t.Fatalf("session/new result missing %q: %s", want, s)
		}
	}
}

// An upstream with no modes must yield a plain {sessionId} downstream session/new (graceful absence).
func TestSessionNewOmitsModesWhenUpstreamHasNone(t *testing.T) {
	addr, _, teardown := startMux(t)
	defer teardown()
	tc := dial(t, addr)
	defer tc.close()
	tc.call(t, "initialize", json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`))
	res := tc.call(t, "session/new", json.RawMessage(`{"cwd":"/app","mcpServers":[]}`))
	if contains(string(res.Result), "modes") {
		t.Fatalf("no upstream modes -> no modes block, got %s", string(res.Result))
	}
}

// A downstream session/set_mode from ANY client must forward upstream (v1: no arbitration), and the
// resulting current_mode_update must fan out to ALL connected clients (cat F).
func TestSetModeForwardsUpstreamAndFansOut(t *testing.T) {
	addr, fa, teardown := startMuxWithModes(t, `{"currentModeId":"build","availableModes":[{"id":"build","name":"Build"},{"id":"plan","name":"Plan"}]}`)
	defer teardown()
	a, b := dial(t, addr), dial(t, addr)
	defer a.close()
	defer b.close()
	a.handshake(t)
	b.handshake(t)

	// Client A switches the shared session's mode.
	a.send(acp.Message{ID: acp.IntID(500), Method: "session/set_mode", Params: json.RawMessage(`{"sessionId":"S1","modeId":"plan"}`)})

	// Upstream received the switch.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if ms := fa.setModeIDs(); len(ms) == 1 {
			if ms[0] != "plan" {
				t.Fatalf("upstream set_mode = %q, want plan", ms[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream never received the set_mode")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// BOTH clients see the current_mode_update fan-out.
	awaitCurrentMode := func(tc *testClient) {
		for {
			m := tc.read(t)
			if m.Method == "session/update" && contains(string(m.Params), `"current_mode_update"`) &&
				contains(string(m.Params), `"currentModeId":"plan"`) {
				return
			}
		}
	}
	awaitCurrentMode(a)
	awaitCurrentMode(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
