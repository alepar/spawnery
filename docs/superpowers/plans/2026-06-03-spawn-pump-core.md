# Per-Spawn Pump Core Implementation Plan (Plan 1 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a hermetic, fully unit-tested `internal/node` **pump** — a long-lived per-spawn component that owns one goose ACP session, fans out an append-only frame log to N clients with resumable per-client cursors, brokers turn/queue, and brokers permission requests — behind a clean interface, with **no wiring** into the live node/CP/web (that's Plan 2, sp-bjd).

**Architecture:** A `Pump` wraps a goose stdio (`io.Writer` stdin + `io.Reader` stdout). On `start()` it does `initialize`+`session/new` (the readiness gate) and launches a reader goroutine (translate ACP→frames, broker turn/queue, append to the log, fan out) and a writer goroutine (sole writer to goose). Clients attach as `(clientID, cursor, send)` subscribers; each gets its own goroutine that ships log frames `> cursor`. Permissions are transient broadcasts with first-response-wins + timeout-deny. Everything behind one mutex; hermetic via `io.Pipe` + capturing senders.

**Tech Stack:** Go 1.26; `internal/acp` (ndjson JSON-RPC: `acp.Message`, `acp.WriteMessage`, `acp.NewReader`/`ReadMessage`).

**Bead:** sp-bjd. **Spec:** `docs/superpowers/specs/2026-06-03-spawn-pump-multiclient-design.md`.

**Conventions:** commit `--no-verify` (beads hook), local-only (NO push). Hermetic Go tests only. New code lives in `internal/node` but is **not referenced by `attach.go` yet** — it compiles standalone.

---

## Interface (the contract Plan 2 wires)

```go
// frameSender delivers one encoded frame line to a client; returns an error if the client is gone.
type frameSender func(line []byte) error

func newPump(stdin io.Writer, stdout io.Reader) *Pump
func (p *Pump) start(ctx context.Context, readyTimeout time.Duration) error // initialize+session/new; ready or err
func (p *Pump) attachClient(clientID string, cursor int64, send frameSender)
func (p *Pump) detachClient(clientID string)
func (p *Pump) fromClient(clientID string, line []byte) // a client->pump frame (prompt / perm_response)
func (p *Pump) stop()
```

## File Structure

- **Create `internal/node/frame.go`** — the pump↔client frame vocabulary (`Frame` struct + encode/decode). One responsibility: the wire codec.
- **Create `internal/node/pump.go`** — the `Pump` (grows across Tasks 2–4): fan-out/log/cursors (T2), agent session + broker + translation (T3), permissions (T4).
- **Create `internal/node/frame_test.go`, `internal/node/pump_test.go`** — hermetic tests.

---

## Task 1: Frame codec

**Files:** Create `internal/node/frame.go`, `internal/node/frame_test.go`

- [ ] **Step 1: Write the failing test** — `internal/node/frame_test.go`:

```go
package node

import (
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Seq: 1, Kind: "user", Text: "hello"},
		{Seq: 2, Kind: "agent", Text: "hi"},
		{Seq: 3, Kind: "thought", Text: "hmm"},
		{Seq: 4, Kind: "tool", ToolID: "t1", Title: "read", Status: "in_progress"},
		{Seq: 5, Kind: "turn", State: "busy", Queued: 2},
		{Kind: "perm_request", ReqID: "p1", Title: "allow fs?"},
		{Kind: "reset", FromSeq: 10},
		{Kind: "prompt", Text: "do it"},
		{Kind: "perm_response", ReqID: "p1", Allow: true},
	}
	for _, c := range cases {
		line := encodeFrame(c)
		if !strings.HasSuffix(string(line), "\n") {
			t.Fatalf("%s: not newline-terminated", c.Kind)
		}
		got, err := decodeFrame(line)
		if err != nil {
			t.Fatalf("%s: decode: %v", c.Kind, err)
		}
		if got != c {
			t.Fatalf("%s: round-trip mismatch: %+v != %+v", c.Kind, got, c)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := decodeFrame([]byte("not json\n")); err == nil {
		t.Fatal("want error on garbage")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/node/ -run TestFrame -v` → undefined `Frame`/`encodeFrame`/`decodeFrame`.

- [ ] **Step 3: Write `internal/node/frame.go`**:

```go
package node

import "encoding/json"

// Frame is one ndjson line on the pump<->client wire. Logged conversation frames carry Seq>0
// (user/agent/thought/tool/turn); transient frames carry Seq==0 (perm_request, reset); client->pump
// frames are prompt / perm_response. A single struct (sparse) keeps the codec trivial; Kind selects.
type Frame struct {
	Seq     int64  `json:"seq,omitempty"`
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`   // user/agent/thought/prompt
	ToolID  string `json:"toolId,omitempty"` // tool
	Title   string `json:"title,omitempty"`  // tool / perm_request
	Status  string `json:"status,omitempty"` // tool
	State   string `json:"state,omitempty"`  // turn: busy|idle
	Queued  int    `json:"queued,omitempty"` // turn
	ReqID   string `json:"reqId,omitempty"`  // perm_request / perm_response
	Allow   bool   `json:"allow,omitempty"`  // perm_response
	FromSeq int64  `json:"fromSeq,omitempty"`// reset
}

func encodeFrame(f Frame) []byte {
	b, _ := json.Marshal(f)
	return append(b, '\n')
}

func decodeFrame(line []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, err
	}
	return f, nil
}
```

- [ ] **Step 4: Run, verify PASS** — `go test ./internal/node/ -run TestFrame -v` (2 pass) and `go vet ./internal/node/`.

- [ ] **Step 5: Commit**
```bash
git add internal/node/frame.go internal/node/frame_test.go
git commit --no-verify -m "feat(node): pump<->client frame codec [sp-bjd]"
```

---

## Task 2: Pump fan-out core (log + clients + cursors + trim)

**Files:** Create `internal/node/pump.go`; Test: `internal/node/pump_test.go`

This task builds ONLY the client/log machinery — no agent yet. `appendFrames` is exercised directly.

- [ ] **Step 1: Write the failing test** — `internal/node/pump_test.go`:

```go
package node

import (
	"sync"
	"testing"
	"time"
)

// capSender collects frames a client received, thread-safe, with a wait helper.
type capSender struct {
	mu   sync.Mutex
	got  []Frame
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
	p.maxLog = 2 // small cap for the test
	a := &capSender{}
	p.appendFrames([]Frame{{Kind: "agent", Text: "1"}}) // seq 1
	p.appendFrames([]Frame{{Kind: "agent", Text: "2"}}) // seq 2
	p.appendFrames([]Frame{{Kind: "agent", Text: "3"}}) // seq 3 -> trims seq 1, base=1
	// a resumes from seq 1, which is below base(1) -> gets a reset{fromSeq:1} then frames 2,3.
	p.attachClient("a", 1, a.send)
	a.waitLen(t, 3)
	if a.got[0].Kind != "reset" || a.got[0].FromSeq != 1 { t.Fatalf("want reset{1} first, got %+v", a.got[0]) }
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/node/ -run 'TestFanout|TestLate|TestDetach|TestReconnect|TestTrim' -v` → undefined `Pump`/`newPump`/`appendFrames`.

- [ ] **Step 3: Write `internal/node/pump.go` (fan-out core)**:

```go
package node

import (
	"io"
	"sync"
)

const defaultMaxLog = 2000 // cap the per-spawn frame log; oldest trimmed (a lagging client gets reset)

type client struct {
	cursor int64           // last seq this client has been sent
	send   frameSender
	notify chan struct{}   // buffered(1): "catch up"
	done   chan struct{}
}

// Pump is the long-lived per-spawn relay: it owns the goose stdio, an append-only frame log, and a
// set of client subscribers. Built across Tasks 2-4. All fields behind mu.
type Pump struct {
	stdin  io.Writer
	stdout io.Reader

	mu      sync.Mutex
	log     []Frame          // log[i].Seq == base+1+i (contiguous)
	base    int64            // seq of the last trimmed frame (0 = nothing trimmed)
	seq     int64            // last assigned seq
	maxLog  int
	clients map[string]*client
	stopped bool
}

func newPump(stdin io.Writer, stdout io.Reader) *Pump {
	return &Pump{stdin: stdin, stdout: stdout, maxLog: defaultMaxLog, clients: map[string]*client{}}
}

// appendFrames assigns seqs, appends to the log (trimming the oldest past maxLog), and wakes clients.
func (p *Pump) appendFrames(fs []Frame) {
	if len(fs) == 0 {
		return
	}
	p.mu.Lock()
	for i := range fs {
		p.seq++
		fs[i].Seq = p.seq
		p.log = append(p.log, fs[i])
	}
	if over := len(p.log) - p.maxLog; over > 0 {
		p.base += int64(over)
		p.log = append(p.log[:0], p.log[over:]...)
	}
	for _, c := range p.clients {
		wake(c)
	}
	p.mu.Unlock()
}

func wake(c *client) {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (p *Pump) attachClient(clientID string, cursor int64, send frameSender) {
	p.mu.Lock()
	if old := p.clients[clientID]; old != nil {
		close(old.done) // replace same id (defensive; normally ids are unique per connection)
	}
	c := &client{cursor: cursor, send: send, notify: make(chan struct{}, 1), done: make(chan struct{})}
	p.clients[clientID] = c
	p.mu.Unlock()
	wake(c) // initial catch-up
	go p.clientLoop(c)
}

func (p *Pump) detachClient(clientID string) {
	p.mu.Lock()
	if c := p.clients[clientID]; c != nil {
		close(c.done)
		delete(p.clients, clientID)
	}
	p.mu.Unlock()
}

// clientLoop ships log frames > c.cursor whenever woken, until done.
func (p *Pump) clientLoop(c *client) {
	for {
		select {
		case <-c.done:
			return
		case <-c.notify:
		}
		for {
			p.mu.Lock()
			// If the client's cursor is below base, it missed trimmed frames -> reset to base.
			var reset *Frame
			if c.cursor < p.base {
				r := Frame{Kind: "reset", FromSeq: p.base}
				reset = &r
				c.cursor = p.base
			}
			var batch []Frame
			if c.cursor < p.seq {
				from := c.cursor - p.base // index of first unseen frame
				batch = append(batch, p.log[from:]...)
				c.cursor = p.seq
			}
			p.mu.Unlock()
			if reset == nil && len(batch) == 0 {
				break
			}
			if reset != nil {
				if c.send(encodeFrame(*reset)) != nil {
					return
				}
			}
			for _, f := range batch {
				if c.send(encodeFrame(f)) != nil {
					return
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run, verify PASS + race** — `go test ./internal/node/ -run 'TestFanout|TestLate|TestDetach|TestReconnect|TestTrim' -race -v` (5 pass, no race); `go vet ./internal/node/`.

- [ ] **Step 5: Commit**
```bash
git add internal/node/pump.go internal/node/pump_test.go
git commit --no-verify -m "feat(node): pump fan-out core (append-only log, per-client cursor, trim/reset) [sp-bjd]"
```

---

## Task 3: Agent session + broker + ACP→frame translation

**Files:** Modify `internal/node/pump.go`; Test: add to `internal/node/pump_test.go`

Adds the goose side: `start()` (handshake/readiness, single owned session), the reader (translate goose ACP lines → frames + broker turn/queue), the writer (sole writer to goose), and `fromClient` for `prompt` frames.

- [ ] **Step 1: Write the failing test** — append to `pump_test.go`:

Add these imports to `pump_test.go` (merge with the existing `sync`/`testing`/`time`):
`"context"`, `"io"`, `"spawnery/internal/acp"`.

```go
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
	// expect: user frame, turn(busy), agent "ECHO", turn(idle) — these kinds must be present.
	a.waitLen(t, 4)
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
	_, gooseOutR := io.Pipe() // stdout that never produces -> initialize never answered
	p := newPump(io.Discard, gooseOutR)
	if err := p.start(context.Background(), 50*time.Millisecond); err == nil {
		t.Fatal("want timeout error")
	}
}
```

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/node/ -run 'TestPromptStreams|TestStartTimes' -v` → undefined `start`/`stop`/`fromClient` + the agent logic.

- [ ] **Step 3: Implement in `pump.go`** — add the agent session, broker (turn/queue), translation, writer, and `fromClient`. Key pieces:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"spawnery/internal/acp"
)

// --- pump agent state (add to the Pump struct) ---
//   sessionID string
//   toAgent   chan []byte          // ndjson lines for the agent-writer (sole writer to stdin)
//   busy      bool                 // a session/prompt turn is in flight
//   queue     []string             // prompt texts held while busy, FIFO
//   nextID    int                  // JSON-RPC id counter for pump->goose requests
//   readerDone chan struct{}
// initialise toAgent (buffered, e.g. 64) and readerDone in newPump.

const maxQueued = 50 // mirror transcript.MaxQueued / web MAX_QUEUED

func (p *Pump) start(ctx context.Context, readyTimeout time.Duration) error {
	go p.writeLoop()                 // sole writer to goose stdin
	go p.readLoop()                  // reads goose stdout forever
	if err := p.handshake(ctx, readyTimeout); err != nil {
		return err
	}
	return nil
}

// handshake sends initialize, awaits the matching response (readiness), then session/new.
func (p *Pump) handshake(ctx context.Context, timeout time.Duration) error {
	initID := p.send(acp.Message{Method: "initialize", Params: []byte(`{"protocolVersion":1,"clientCapabilities":{}}`)})
	if err := p.awaitResult(initID, timeout); err != nil {
		return fmt.Errorf("agent not ready: %w", err)
	}
	newID := p.send(acp.Message{Method: "session/new", Params: []byte(`{"cwd":"/app","mcpServers":[]}`)})
	res, err := p.awaitResultMsg(newID, timeout)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var r struct{ SessionID string `json:"sessionId"` }
	_ = json.Unmarshal(res, &r)
	p.mu.Lock(); p.sessionID = r.SessionID; p.mu.Unlock()
	return nil
}
```

The implementer fills in:
- **`send(msg) int`** — assigns `nextID`, marshals with `acp.WriteMessage` into `toAgent`, returns the id.
- **`awaitResult(id, timeout)` / `awaitResultMsg`** — register a one-shot waiter (a `map[int]chan acp.Message` under `mu`); `readLoop` resolves it when it sees a matching `id` result; select on a timer + ctx. (Same shape as sp-39u's `awaitInitialize`, generalized.)
- **`writeLoop()`** — `for line := range p.toAgent { p.stdin.Write(line) }`; exits on `stop()` (close `toAgent` or select on a done channel).
- **`readLoop()`** — `rd := acp.NewReader(p.stdout)`; for each `acp.Message`:
  - If it has an `ID` matching a pending waiter → resolve it (handshake/result path), **do not** log/fan-out.
  - Else translate + broker (`onAgentMessage`).
- **`onAgentMessage(m)`** — turn `m` into 0+ frames and broker turn-end:
  - `session/update` → `agent`/`thought`/`tool` frame (parse `update.sessionUpdate`): `agent_message_chunk`→`{Kind:"agent",Text:content.text}`, `agent_thought_chunk`→`thought`, `tool_call`/`tool_call_update`→`tool`. `appendFrames` them.
  - a result with `id == inFlightPromptID` → **turn-end**: under `mu` set `busy=false`, emit `turn{idle, queued:len(queue)}`; then drain one queued prompt (forward it, set `busy=true`, emit `user` + `turn{busy}`). `appendFrames` the turn/user frames; `send` the drained prompt to goose.
  - `session/request_permission` → Task 4 (broadcast). For now route nothing.
- **`fromClient(clientID, line)`** — `decodeFrame`; if `Kind=="prompt"`: under `mu`, if `!busy` → set `busy=true`, send `session/prompt{sessionId, prompt:[{type:text,text}]}` to goose (`p.send(...)`, record its id as `inFlightPromptID`), `appendFrames([user{text}, turn{busy}])`; else if `len(queue)<maxQueued` → `queue=append(queue,text)`, `appendFrames([user{text}, turn{busy, queued:len(queue)}])` (the user frame for a queued prompt is still logged so all clients see it). (`perm_response` → Task 4.)
- **`stop()`** — set `stopped`, close the agent attach path (close `toAgent`/cancel `readLoop`), close all client `done`.

Build the `session/prompt` params with a small helper:
```go
func promptParams(sessionID, text string) []byte {
	b, _ := json.Marshal(map[string]any{"sessionId": sessionID, "prompt": []any{map[string]string{"type": "text", "text": text}}})
	return b
}
```

- [ ] **Step 4: Run, verify PASS + race** — `go test ./internal/node/ -race -count=1` (all node tests incl. T1/T2); `go vet ./internal/node/`. Expected: green, no race.

- [ ] **Step 5: Commit**
```bash
git add internal/node/pump.go internal/node/pump_test.go
git commit --no-verify -m "feat(node): pump agent session + broker + ACP translation (single owned session) [sp-bjd]"
```

---

## Task 4: Permissions (broadcast / first-wins / resend-on-attach / timeout-deny)

**Files:** Modify `internal/node/pump.go`; Test: add to `internal/node/pump_test.go`

- [ ] **Step 1: Write the failing test** — append to `pump_test.go`. Extend `scriptGoose` with a `permPrompt` mode: when it receives a `session/prompt` whose text is `"need-perm"`, it emits a `session/request_permission` (with an id) instead of an immediate result, and only emits the `end_turn` result after it receives the permission response. (Implementer: add a branch in `scriptGoose` that, on `session/request_permission` *response* — a client→agent message the pump forwards — finishes the turn.) Then:

```go
func TestPermissionBroadcastFirstWins(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoosePerm(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	a, b := &capSender{}, &capSender{}
	p.attachClient("a", 0, a.send)
	p.attachClient("b", 0, b.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "need-perm"}))
	// both clients receive a perm_request (transient, no seq)
	waitPerm := func(c *capSender) Frame {
		deadline := time.Now().Add(2 * time.Second)
		for {
			c.mu.Lock()
			for _, f := range c.got { if f.Kind == "perm_request" { c.mu.Unlock(); return f } }
			c.mu.Unlock()
			if time.Now().After(deadline) { t.Fatal("no perm_request"); }
			time.Sleep(5 * time.Millisecond)
		}
	}
	pa := waitPerm(a); waitPerm(b)
	if pa.Seq != 0 { t.Fatalf("perm_request must be transient (seq 0), got %d", pa.Seq) }
	// b answers first -> turn completes; a's later answer is a no-op.
	p.fromClient("b", encodeFrame(Frame{Kind: "perm_response", ReqID: pa.ReqID, Allow: true}))
	p.fromClient("a", encodeFrame(Frame{Kind: "perm_response", ReqID: pa.ReqID, Allow: false}))
	// a turn(idle) eventually arrives for both
	waitIdle := func(c *capSender) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			c.mu.Lock()
			for _, f := range c.got { if f.Kind == "turn" && f.State == "idle" { c.mu.Unlock(); return } }
			c.mu.Unlock()
			if time.Now().After(deadline) { t.Fatal("no idle turn"); }
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitIdle(a); waitIdle(b)
}

func TestPermissionResentOnAttachWhilePending(t *testing.T) {
	gooseInR, gooseInW := io.Pipe()
	gooseOutR, gooseOutW := io.Pipe()
	go scriptGoosePerm(gooseInR, gooseOutW)
	p := newPump(gooseInW, gooseOutR)
	if err := p.start(context.Background(), 2*time.Second); err != nil { t.Fatal(err) }
	defer p.stop()
	a := &capSender{}
	p.attachClient("a", 0, a.send)
	p.fromClient("a", encodeFrame(Frame{Kind: "prompt", Text: "need-perm"}))
	// wait for a to get the perm_request, THEN a late client b attaches and must also get it.
	for { a.mu.Lock(); has := false; for _, f := range a.got { if f.Kind == "perm_request" { has = true } }; a.mu.Unlock(); if has { break }; time.Sleep(5*time.Millisecond) }
	b := &capSender{}
	p.attachClient("b", 0, b.send)
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock(); has := false; for _, f := range b.got { if f.Kind == "perm_request" { has = true } }; b.mu.Unlock()
		if has { break }
		if time.Now().After(deadline) { t.Fatal("late client did not get pending perm_request") }
		time.Sleep(5 * time.Millisecond)
	}
}
```

(Implementer: write `scriptGoosePerm` — like `scriptGoose` but on `session/prompt` text `"need-perm"` it emits `{"method":"session/request_permission","id":99,"params":{"options":[{"optionId":"allow","kind":"allow"},{"optionId":"deny","kind":"reject"}]}}` and waits to see the pump forward a response for id 99 before emitting the `end_turn` result.)

- [ ] **Step 2: Run, verify FAIL** — `go test ./internal/node/ -run TestPermission -v`.

- [ ] **Step 3: Implement permissions in `pump.go`**:
- Add to `Pump`: `pending map[string]*pendingPerm` where `pendingPerm{ agentID *int (the goose request id), options json.RawMessage, deadline timer }`. Add a `permTimeout` field (default e.g. 2 min; settable for tests).
- In `readLoop`/`onAgentMessage`: a `session/request_permission` → mint a `reqID` (e.g. `fmt.Sprint(agentID)`), store `pending[reqID]`, **broadcast** a transient `perm_request{ReqID, Title (from options/text)}` to all clients (a new `broadcastTransient(f Frame)` that sends to every client's `send` directly — NOT via the log/`appendFrames`), arm a timeout that auto-denies.
- In `attachClient`: after registering, under `mu` **re-send** every `pending` perm_request to the new client's `send` (so a late client sees it). (Do this right after adding to `clients`, before/around the initial `wake`.)
- In `fromClient`: `Kind=="perm_response"` → under `mu` look up `pending[ReqID]`; if present, remove it, stop its timer, and forward the chosen option to goose as the permission response (`p.send` a result for the goose request id with `{"outcome":{"outcome":"selected","optionId": <allow/deny option>}}`), mapping `Allow` to an allow-ish vs reject-ish option (reuse the pick logic from `web/src/acp/client.ts handlePermission`). If absent (already resolved) → no-op.
- Timeout → auto-deny (same as a deny response) + remove pending.

`broadcastTransient` (note: transient frames carry `Seq==0` and are NOT appended to the log):
```go
func (p *Pump) broadcastTransient(f Frame) {
	line := encodeFrame(f)
	p.mu.Lock()
	for _, c := range p.clients {
		_ = c.send(line) // best-effort; a dead client is reaped by its own loop/detach
	}
	p.mu.Unlock()
}
```
(Implementer: sending under `mu` is acceptable here since `send` at the integration is a non-blocking channel write; keep it consistent with how `clientLoop` is structured, or push to each client's loop via a transient queue if you prefer — the tests only require delivery.)

- [ ] **Step 4: Run, verify PASS + race + full node suite** — `go test ./internal/node/ -race -count=1`; `go vet ./internal/node/`; `go build ./...`. Expected: all green, no race.

- [ ] **Step 5: Commit**
```bash
git add internal/node/pump.go internal/node/pump_test.go
git commit --no-verify -m "feat(node): pump permission broker (broadcast, first-wins, resend-on-attach, timeout-deny) [sp-bjd]"
```

---

## After Plan 1

The pump is a tested standalone unit. **Plan 2 (integration)** wires `newPump` into `attach.go` (replacing `openSession`/`closeSession`/`brokerEndpoint`), adds `client_id`+`cursor` to the `nodev1` proto, makes the CP router + `ws.go` multi-client, and converts the web client to the thin frame protocol — then the e2e + host validation. I'll write Plan 2 after this lands.

**Note on `broadcastTransient` vs `clientLoop` send concurrency:** Tasks 2–4 send to a client from two places (the client's own `clientLoop` goroutine and `broadcastTransient`). The integration's `frameSender` (Plan 2) will be a non-blocking channel write, so concurrent sends are safe; for the hermetic tests, `capSender` is mutex-guarded. If the final review prefers a single-writer-per-client invariant, route transient frames through a per-client transient queue drained by `clientLoop` — call it out in review.
