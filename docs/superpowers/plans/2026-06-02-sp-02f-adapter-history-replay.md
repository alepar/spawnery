# Adapter History Replay (Slice 2, sp-02f) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the in-container `acpadapter` an ACP-aware recording proxy that replays a spawn's transcript to any (re)connecting client via a `spawn/history` frame, and teach the web client to consume it.

**Architecture:** The adapter parses the newline-delimited JSON (ndjson) ACP stream in both directions, forwards every line byte-for-byte (lossless), and records a coalesced transcript (`session/prompt` → user items; `session/update` → agent/thought/tool items). On each client attach it writes one out-of-band `{"method":"spawn/history","params":{"items":[…]}}` notification before resuming forwarding. The web `Client` gains an `onHistory` callback + a `spawn/history` route branch + a pure mapper to the chat `Item` shape.

**Tech Stack:** Go (the adapter, `package main` under `deploy/agent/acpadapter/`), TypeScript/Vitest (web client).

**Conventions:**
- Commits use `git commit --no-verify` (beads hook).
- Local-only repo, no push. Do NOT touch `.beads/`.
- Hermetic tests only: Go unit tests for the adapter (no Docker), Vitest for the client. Image rebuild (`make images`) is host-gated and NOT part of this slice's tests.
- Reference spec: `docs/superpowers/specs/2026-06-02-spawn-lifecycle-ui-design.md` §Slice 2.

**Key existing facts (do not re-derive):**
- Wire framing is ndjson (`web/src/acp/conn.ts`): one JSON object per `\n`-terminated line, each direction.
- `deploy/agent/acpadapter/bridge.go` today: `connHub{mu, cur}` (current client conn), `pump` (single 32 KB byte reader of agent stdout → `hub.write`), `serve` (accept loop: `hub.set(conn)` then `io.Copy(toAgent, conn)`; conn stays attached on stdin EOF for half-close). `main.go` starts the agent subprocess and calls `serve`.
- Existing adapter tests (`bridge_test.go`, `main_test.go`) drive NON-JSON bytes (`"hello\n"`, `"ping\n"`, `"echo-me\n"`) through an echo/`cat` agent and assert byte-exact round-trips + persistence across reconnect + half-close. These MUST stay green — parse failures must be non-fatal and forwarding byte-exact.
- ACP message shapes: client→agent `{"method":"session/prompt","params":{"sessionId":...,"prompt":[{"type":"text","text":"..."}]}}`. agent→client `{"method":"session/update","params":{"sessionId":...,"update":{"sessionUpdate":"agent_message_chunk"|"agent_thought_chunk"|"tool_call"|"tool_call_update","content":{"type":"text","text":"..."},"toolCallId":"...","title":"...","status":"..."}}}` (`web/src/acp/types.ts`).
- Web chat `Item` union (`web/src/views/chat/types.ts`): `{kind:"user"|"agent"|"thought", text}` and `{kind:"tool", title, status?}` (each also has a numeric `id`).
- Web `Client` (`web/src/acp/client.ts`): `route(m)` dispatches `session/update` → handlers, `session/request_permission` → permission, else resolves a pending call by `id`. A `spawn/history` notification handled in `route` fires regardless of in-flight prompt state.

---

### Task 1: Adapter transcript recorder (pure, unit-tested)

**Files:**
- Create: `deploy/agent/acpadapter/record.go`
- Test: `deploy/agent/acpadapter/record_test.go`

This task builds ONLY the recorder data structure + parsing/coalescing + frame serialization. It is not wired into the proxy yet (Task 2).

- [ ] **Step 1: Write the failing test**

Create `deploy/agent/acpadapter/record_test.go`:

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// parse the spawn/history frame the recorder emits back into items for assertions.
func decodeFrame(t *testing.T, frame []byte) []item {
	t.Helper()
	if len(frame) == 0 {
		t.Fatal("expected a non-empty history frame")
	}
	if frame[len(frame)-1] != '\n' {
		t.Fatalf("frame must be newline-terminated, got %q", string(frame))
	}
	var m struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []item `json:"items"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("frame not valid json: %v\n%s", err, string(frame))
	}
	if m.Jsonrpc != "2.0" || m.Method != "spawn/history" {
		t.Fatalf("frame envelope wrong: jsonrpc=%q method=%q", m.Jsonrpc, m.Method)
	}
	return m.Params.Items
}

func clientPrompt(text string) []byte {
	return []byte(`{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"` + text + `"}]}}` + "\n")
}
func agentChunk(kind, text string) []byte {
	return []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"` + kind + `","content":{"type":"text","text":"` + text + `"}}}}` + "\n")
}

func TestRecorderCoalescesTranscript(t *testing.T) {
	r := newRecorder()
	r.observeClient(clientPrompt("hello"))
	r.observeAgent(agentChunk("agent_message_chunk", "He"))
	r.observeAgent(agentChunk("agent_message_chunk", "llo!")) // coalesces with previous agent item
	r.observeAgent(agentChunk("agent_thought_chunk", "hmm"))
	r.observeAgent([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"search","status":"pending"}}}` + "\n"))
	r.observeAgent([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}` + "\n"))

	items := decodeFrame(t, r.historyFrame())
	want := []item{
		{Role: "user", Text: "hello"},
		{Role: "agent", Text: "Hello!"},
		{Role: "thought", Text: "hmm"},
		{Role: "tool", Title: "search", Status: "completed"},
	}
	if len(items) != len(want) {
		t.Fatalf("items=%+v want %+v", items, want)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Fatalf("item[%d]=%+v want %+v", i, items[i], want[i])
		}
	}
}

func TestRecorderIgnoresNonAcpAndEmptyIsNilFrame(t *testing.T) {
	r := newRecorder()
	if f := r.historyFrame(); f != nil {
		t.Fatalf("empty recorder must yield a nil frame, got %q", string(f))
	}
	r.observeClient([]byte("not json\n"))                       // ignored
	r.observeAgent([]byte("hello\n"))                            // ignored (non-json)
	r.observeAgent([]byte(`{"method":"initialize","id":1}` + "\n")) // ignored (not session/update)
	if f := r.historyFrame(); f != nil {
		t.Fatalf("recorder must stay empty for non-transcript traffic, got %q", string(f))
	}
}

func TestRecorderCapsAndMarksTruncation(t *testing.T) {
	r := newRecorder()
	for i := 0; i < maxHistoryItems+50; i++ {
		r.observeClient(clientPrompt("p")) // each prompt is its own user item (distinct turns)
	}
	items := decodeFrame(t, r.historyFrame())
	if len(items) != maxHistoryItems {
		t.Fatalf("len=%d want capped at %d", len(items), maxHistoryItems)
	}
	if items[0].Role != "system" || !strings.Contains(items[0].Text, "truncated") {
		t.Fatalf("first item must be the truncation marker, got %+v", items[0])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./deploy/agent/acpadapter/ -run TestRecorder -count=1`
Expected: FAIL — `undefined: newRecorder`, `undefined: item`, `undefined: maxHistoryItems`.

- [ ] **Step 3: Implement the recorder**

Create `deploy/agent/acpadapter/record.go`:

```go
package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// maxHistoryItems caps the in-memory transcript. Past the cap the oldest items are dropped and a
// single leading "truncated" marker is kept. On-demand pagination for long histories is a post-demo
// epic (sp-suc).
const maxHistoryItems = 500

// item is one transcript entry. It marshals directly into the spawn/history frame. Roles:
// user | agent | thought | tool | system (system = the truncation marker).
type item struct {
	Role   string `json:"role"`
	Text   string `json:"text,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// recorder accumulates a coalesced transcript from the ACP traffic flowing through the adapter.
// All methods are safe for concurrent use (the agent-side pump and the client-side copy both call in).
type recorder struct {
	mu      sync.Mutex
	items   []item
	toolIdx map[string]int // toolCallId -> index in items, for tool_call_update
}

func newRecorder() *recorder { return &recorder{toolIdx: map[string]int{}} }

// observeClient records a client→agent line if it is a session/prompt (one user item per prompt).
func (r *recorder) observeClient(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/prompt" {
		return
	}
	var sb strings.Builder
	for _, p := range m.Params.Prompt {
		sb.WriteString(p.Text)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.push(item{Role: "user", Text: sb.String()})
}

// observeAgent records an agent→client line if it is a session/update notification.
func (r *recorder) observeAgent(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
				ToolCallID string `json:"toolCallId"`
				Title      string `json:"title"`
				Status     string `json:"status"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return
	}
	u := m.Params.Update
	r.mu.Lock()
	defer r.mu.Unlock()
	switch u.SessionUpdate {
	case "agent_message_chunk":
		r.appendChunk("agent", u.Content.Text)
	case "agent_thought_chunk":
		r.appendChunk("thought", u.Content.Text)
	case "tool_call":
		r.push(item{Role: "tool", Title: u.Title, Status: u.Status})
		if u.ToolCallID != "" {
			r.toolIdx[u.ToolCallID] = len(r.items) - 1
		}
	case "tool_call_update":
		if i, ok := r.toolIdx[u.ToolCallID]; ok && i >= 0 && i < len(r.items) {
			r.items[i].Status = u.Status
		}
	}
}

// appendChunk coalesces consecutive same-role chunks into one item (mirrors the web client's
// appendChunk). Caller holds r.mu.
func (r *recorder) appendChunk(role, text string) {
	if text == "" {
		return
	}
	if n := len(r.items); n > 0 && r.items[n-1].Role == role {
		r.items[n-1].Text += text
		return
	}
	r.push(item{Role: role, Text: text})
}

// push appends an item and enforces the cap. Caller holds r.mu.
func (r *recorder) push(it item) {
	r.items = append(r.items, it)
	if len(r.items) <= maxHistoryItems {
		return
	}
	// Drop oldest, keep a single leading truncation marker. tool index positions are invalidated by
	// the slice, so reset it (a tool_call_update for a pre-truncation tool simply won't apply — an
	// acceptable edge at 500+ items; pagination is the post-demo epic).
	over := len(r.items) - maxHistoryItems
	trimmed := append([]item{{Role: "system", Text: "earlier history truncated"}}, r.items[over+1:]...)
	r.items = trimmed
	r.toolIdx = map[string]int{}
}

// historyFrame returns a newline-terminated spawn/history JSON-RPC notification snapshotting the
// current transcript, or nil if the transcript is empty (nothing to replay).
func (r *recorder) historyFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]item, len(r.items))
	copy(snap, r.items)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []item `json:"items"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/history"
	env.Params.Items = snap
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./deploy/agent/acpadapter/ -run TestRecorder -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deploy/agent/acpadapter/record.go deploy/agent/acpadapter/record_test.go
git commit --no-verify -m "feat(acpadapter): transcript recorder + spawn/history frame (sp-02f)"
```

---

### Task 2: Wire the recorder into the proxy (line-oriented, replay-on-attach)

**Files:**
- Modify: `deploy/agent/acpadapter/bridge.go`
- Test: `deploy/agent/acpadapter/bridge_test.go` (append an integration test; existing tests must stay green)

- [ ] **Step 1: Write the failing integration test**

Append to `deploy/agent/acpadapter/bridge_test.go`:

```go
// A reconnecting client must receive a spawn/history frame replaying the transcript that flowed
// through the adapter while a PRIOR client was attached. Uses a passthrough agent (cat): the client
// "prompt" line is echoed back by the agent as-is, but the recorder also observes it as a user item.
func TestServeReplaysHistoryOnReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Agent that, for each line in, emits a session/update agent_message_chunk line out — so the
	// adapter records BOTH a user item (from the client's session/prompt) and an agent item.
	toAgent, fromAgent := scriptedAgent()
	go serve(ln, toAgent, fromAgent)

	// First client: send a session/prompt; the scripted agent replies with an agent chunk.
	c1, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	prompt := `{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}` + "\n"
	if _, err := io.WriteString(c1, prompt); err != nil {
		t.Fatal(err)
	}
	// drain the agent's reply on c1 so the recorder has observed it before we reconnect.
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(c1).ReadString('\n'); err != nil {
		t.Fatalf("c1 read agent reply: %v", err)
	}
	_ = c1.Close()

	// Reconnect: the FIRST bytes the new client receives must be the spawn/history frame.
	c2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(c2).ReadString('\n')
	if err != nil {
		t.Fatalf("c2 read history: %v", err)
	}
	var m struct {
		Method string `json:"method"`
		Params struct {
			Items []item `json:"items"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("history frame not json: %v\n%s", err, line)
	}
	if m.Method != "spawn/history" {
		t.Fatalf("first frame method=%q want spawn/history", m.Method)
	}
	// must contain the user prompt AND the agent reply.
	var roles []string
	for _, it := range m.Params.Items {
		roles = append(roles, it.Role)
	}
	if len(m.Params.Items) < 2 || m.Params.Items[0].Role != "user" || m.Params.Items[0].Text != "hi" {
		t.Fatalf("history items wrong: %+v (roles=%v)", m.Params.Items, roles)
	}
	hasAgent := false
	for _, it := range m.Params.Items {
		if it.Role == "agent" {
			hasAgent = true
		}
	}
	if !hasAgent {
		t.Fatalf("history must include the agent reply, got roles=%v", roles)
	}
}

// scriptedAgent echoes each newline-delimited input line back as an agent_message_chunk session/update
// line, so traffic in both directions is valid ACP for the recorder. Lives for the whole test.
func scriptedAgent() (io.Writer, io.Reader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		br := bufio.NewReader(inR)
		for {
			_, err := br.ReadString('\n')
			if err != nil {
				_ = outW.Close()
				return
			}
			_, _ = io.WriteString(outW, `{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ack"}}}}`+"\n")
		}
	}()
	return inW, outR
}
```

Note: this test needs `bufio`, `encoding/json`, `io`, `net`, `time`, `filepath`, `testing` imported in `bridge_test.go`. The file already imports `bufio`, `io`, `net`, `path/filepath`, `testing`, `time` (from existing tests). ADD `encoding/json` to that import block.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./deploy/agent/acpadapter/ -run TestServeReplaysHistoryOnReconnect -count=1`
Expected: FAIL — the reconnecting client receives the agent's `ack` (or nothing), not a `spawn/history` frame, because `serve`/`pump` don't record or replay yet.

- [ ] **Step 3: Rewrite `bridge.go` as a recording proxy**

Replace the ENTIRE contents of `deploy/agent/acpadapter/bridge.go` with:

```go
package main

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// connHub holds the currently-attached client connection (at most one) and serializes all writes to
// it so multi-byte frames (agent output lines AND the replayed history frame) never interleave.
type connHub struct {
	mu      sync.Mutex // guards cur
	cur     net.Conn
	writeMu sync.Mutex // serializes writes to cur; never held while waiting on mu
}

// write sends p to the current client (if any) as one atomic write. Output produced while no client
// is attached is dropped (attach/detach semantics).
func (h *connHub) write(p []byte) {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c == nil {
		return
	}
	h.writeMu.Lock()
	_, _ = c.Write(p) // a dead conn's Write returns fast
	h.writeMu.Unlock()
}

// attach makes c the current connection and, holding writeMu so no pump write can slip in front,
// writes the history frame to it FIRST. Returns the displaced connection (if any) for the caller to
// close. A superseded conn is closed only on a new attach, never on stdin EOF (half-close).
//
// Lock order: attach takes writeMu THEN mu (briefly). write takes mu, releases it, THEN takes
// writeMu — so no goroutine holds mu while waiting on writeMu, and there is no deadlock cycle.
func (h *connHub) attach(c net.Conn, history []byte) net.Conn {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	prev := h.cur
	h.cur = c
	h.mu.Unlock()
	if len(history) > 0 && c != nil {
		_, _ = c.Write(history)
	}
	if prev == c {
		return nil
	}
	return prev
}

// pump is the single persistent reader of the agent's stdout. It reads ndjson lines, records any
// session/update into rec, and forwards each line byte-for-byte to the current client. Non-JSON or
// non-ACP lines are forwarded unchanged and simply not recorded.
func pump(fromAgent io.Reader, hub *connHub, rec *recorder) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			rec.observeAgent(line)
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// recordingCopy forwards the client's stdin to the agent line-by-line (byte-for-byte), recording any
// session/prompt into rec. Returns on the client's write-side EOF (full close OR CloseWrite).
func recordingCopy(toAgent io.Writer, conn io.Reader, rec *recorder) {
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := toAgent.Write(line); werr != nil {
				return
			}
			rec.observeClient(line)
		}
		if err != nil {
			return
		}
	}
}

// serve accepts one client at a time and bridges it to the long-lived agent stdio, recording the
// transcript and replaying it (spawn/history) to each newly-attached client. The agent persists
// across client disconnects, so a reconnecting client resumes the same session and gets its history.
// It returns only when the listener is closed.
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	rec := newRecorder()
	go pump(fromAgent, hub, rec)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn, rec.historyFrame()); prev != nil {
			_ = prev.Close()
		}
		recordingCopy(toAgent, conn, rec)
	}
}
```

- [ ] **Step 4: Run the new test to verify it passes**

Run: `go test ./deploy/agent/acpadapter/ -run TestServeReplaysHistoryOnReconnect -count=1`
Expected: PASS.

- [ ] **Step 5: Run the FULL adapter suite (existing byte-exact tests must stay green) + race**

Run:
```bash
go test ./deploy/agent/acpadapter/ -count=1
go test -race ./deploy/agent/acpadapter/ -count=1
```
Expected: PASS for both. The existing `TestServeBridgesAndPersistsAcrossReconnect`, `TestServeClientHalfCloseStopsStdinNotStdout`, and `TestAdapterBinaryBridgesToStubAgent` stay green: their traffic is non-JSON so the recorder stays empty, `historyFrame()` returns nil, and no frame is injected — byte-exact round-trips are preserved.

- [ ] **Step 6: Commit**

```bash
git add deploy/agent/acpadapter/bridge.go deploy/agent/acpadapter/bridge_test.go
git commit --no-verify -m "feat(acpadapter): record + replay transcript on client attach (sp-02f)"
```

---

### Task 3: Web client — `onHistory` + `spawn/history` route + mapper

**Files:**
- Modify: `web/src/acp/types.ts` (add `HistoryItem`)
- Modify: `web/src/acp/client.ts` (add `onHistory` + route branch + exported mapper)
- Test: `web/src/acp/client.test.ts` (append)

- [ ] **Step 1: Write the failing test**

Append to `web/src/acp/client.test.ts` (inside the existing `describe("Client", ...)` block, before its closing `});`):

```typescript
  it("delivers a spawn/history frame to onHistory", () => {
    const ws = new FakeWS();
    const c = new Client(ws);
    const got: any[] = [];
    c.onHistory = (items) => got.push(...items);
    ws.inject({
      method: "spawn/history",
      params: {
        items: [
          { role: "user", text: "hi" },
          { role: "agent", text: "Hello!" },
          { role: "tool", title: "search", status: "completed" },
        ],
      },
    });
    expect(got.length).toBe(3);
    expect(got[0]).toEqual({ role: "user", text: "hi" });
  });
```

And append a second test for the mapper (after the `describe("Client", ...)` block, at top level of the file):

```typescript
import { historyToItems } from "./client";

describe("historyToItems", () => {
  it("maps adapter history items to chat items (system marker -> agent)", () => {
    const out = historyToItems([
      { role: "user", text: "hi" },
      { role: "agent", text: "Hello!" },
      { role: "thought", text: "hmm" },
      { role: "tool", title: "search", status: "completed" },
      { role: "system", text: "earlier history truncated" },
    ]);
    expect(out).toEqual([
      { kind: "user", text: "hi" },
      { kind: "agent", text: "Hello!" },
      { kind: "thought", text: "hmm" },
      { kind: "tool", title: "search", status: "completed" },
      { kind: "agent", text: "earlier history truncated" },
    ]);
  });
});
```

Note: `describe`/`it`/`expect` are already imported at the top of `client.test.ts` (`import { describe, it, expect } from "vitest";`). Add the `import { historyToItems } from "./client";` line near the existing imports (or inline as shown — Vitest hoists it).

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/acp/client.test.ts`
Expected: FAIL — `onHistory` is not delivered (no route branch) and `historyToItems` is not exported.

- [ ] **Step 3: Add the `HistoryItem` type**

In `web/src/acp/types.ts`, append:

```typescript
// spawn/history replay item (mirrors the in-container acpadapter's transcript item).
export interface HistoryItem {
  role: "user" | "agent" | "thought" | "tool" | "system";
  text?: string;
  title?: string;
  status?: string;
}
```

- [ ] **Step 4: Add `onHistory`, the route branch, and the mapper to the client**

In `web/src/acp/client.ts`:

(a) Update the type import to include `HistoryItem`:
```typescript
import type { Message, SessionUpdate, HistoryItem } from "./types";
```

(b) Add an `onHistory` public field to the `Client` class (next to `handlers`):
```typescript
  private handlers: PromptHandlers = {};
  // Replayed transcript from the in-container adapter on (re)connect. Settable by the caller;
  // fires independently of any in-flight prompt (handled directly in route()).
  onHistory?: (items: HistoryItem[]) => void;
```

(c) Add a `spawn/history` branch at the TOP of `route(m)` (before the `session/update` check):
```typescript
  private route(m: Message) {
    if (m.method === "spawn/history") {
      this.onHistory?.((m.params?.items as HistoryItem[]) ?? []);
      return;
    }
    if (m.method === "session/update") {
```

(d) At the END of `client.ts` (after the `Client` class), export the mapper:
```typescript
import type { Item } from "../views/chat/types";

// historyToItems maps replayed adapter history items to chat Items (without ids — the caller assigns
// stable ids). The adapter's "system" marker (e.g. the truncation notice) renders as a plain agent line.
export function historyToItems(items: HistoryItem[]): Omit<Item, "id">[] {
  return items.map((h): Omit<Item, "id"> => {
    switch (h.role) {
      case "user":
        return { kind: "user", text: h.text ?? "" };
      case "thought":
        return { kind: "thought", text: h.text ?? "" };
      case "tool":
        return { kind: "tool", title: h.title ?? "tool", status: h.status };
      case "agent":
      case "system":
      default:
        return { kind: "agent", text: h.text ?? "" };
    }
  });
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd web && npx vitest run src/acp/client.test.ts`
Expected: PASS.

- [ ] **Step 6: Run the full web unit suite (no regressions)**

Run: `cd web && npm test`
Expected: PASS — the existing client/conn/shell tests stay green.

- [ ] **Step 7: Commit**

```bash
git add web/src/acp/types.ts web/src/acp/client.ts web/src/acp/client.test.ts
git commit --no-verify -m "feat(web): client onHistory + spawn/history route + historyToItems mapper (sp-02f)"
```

---

## Definition of Done (Slice 2)

- `acpadapter` records the ACP transcript (coalesced) and replays it as a `spawn/history` frame on each client attach; forwarding remains byte-exact; existing adapter tests + new replay test green (incl. `-race`).
- Web `Client` delivers `spawn/history` to `onHistory`; `historyToItems` maps to chat `Item`s; `HistoryItem` type added; web unit suite green.
- Visual transcript only (no agent memory); lost on suspend (container destroyed) — per spec.

## Out of scope (Slice 3 / later)

- Wiring `onHistory`/`historyToItems` into `App.tsx` to repopulate a spawn's transcript on switch (Slice 3, sp-r6b).
- Multi-spawn UI, status dots, kebab actions (Slice 3).
- Rebuilding the goose/stubagent images with the new adapter (`make images`) — host-gated; happens at slice-3 e2e / manual `just dev`.
- Durable/cross-suspend history + pagination (post-demo epics sp-3nb/sp-suc).
