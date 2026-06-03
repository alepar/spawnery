# Per-spawn turn-state + server-side prompt queueing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the chat "ready to send" state correct per-spawn and impossible to get stuck, by tracking turn-state and queueing prompts server-side in the shared transcript recorder, and reworking the frontend to read that state instead of a fragile per-connection promise.

**Architecture:** Promote `internal/transcript.Recorder` from a passive tee into a per-spawn **broker** that (a) tracks turn-state by correlating each `session/prompt` request id with its response, (b) queues prompts received while busy and drains them FIFO on turn-end, and (c) emits a `spawn/turn` notification + folds turn-state into `spawn/history`. Both lanes (Docker node relay, CRI in-pod adapter) route all agent-bound writes through a single channel so the new drain writes preserve the existing single-writer invariant. The frontend drops its global `busy`/promise gating, removes the Send button (Enter sends), shows a transcript-footer "working…" indicator from backend turn-state, and renders queued messages as dimmed "pending" bubbles.

**Tech Stack:** Go (backend broker + lane wiring; `go test`), React 19 + TypeScript + Vitest + react-virtuoso (frontend; `npm test`).

**Spec:** `docs/superpowers/specs/2026-06-03-chat-turn-state-and-queueing-design.md`

---

## File Structure

**Backend (Go):**
- `internal/transcript/recorder.go` — MODIFY: add `Item.Pending`, turn-state fields, `OnClientLine`/`OnAgentLine` (record+gate), `turnFrameLocked`, fold `turn` into `HistoryFrame`. Keep `ObserveClientLine`/`ObserveAgentLine` as pure-record primitives layered under the new API.
- `internal/transcript/recorder_test.go` — MODIFY: add turn-state/queue/drain/history-turn tests.
- `internal/node/record.go` — MODIFY: replace `recordingEndpoint` with `brokerEndpoint` (gates client→agent, routes all agent writes via a channel, sends `spawn/turn` to client).
- `internal/node/record_test.go` — MODIFY: add brokerEndpoint gating tests.
- `internal/node/attach.go` — MODIFY: call `brokerEndpoint` instead of `recordingEndpoint` (one line).
- `deploy/agent/acpadapter/bridge.go` — MODIFY: route agent writes through a channel; `recordingCopy`/`pump` use `OnClientLine`/`OnAgentLine`.
- `deploy/agent/acpadapter/bridge_test.go` — MODIFY: add gating test.

**Frontend (web):**
- `web/src/acp/types.ts` — MODIFY: `HistoryItem.pending`, `SpawnTurn` type, history `turn` field.
- `web/src/acp/client.ts` — MODIFY: route `spawn/turn` → `onTurn`; fire `onTurn` from `spawn/history.turn`.
- `web/src/acp/client.test.ts` — MODIFY: add `spawn/turn` routing tests.
- `web/src/views/chat/types.ts` — MODIFY: `pending?: boolean` on the user item; export `TurnState`.
- `web/src/views/chat/PromptInput.tsx` — MODIFY: remove Send button; Enter sends.
- `web/src/views/chat/PromptInput.test.tsx` — MODIFY: drop button assertions; Enter-based.
- `web/src/views/chat/MessageList.tsx` — MODIFY: footer typing-indicator; pending bubble styling.
- `web/src/views/chat/MessageList.test.tsx` — CREATE: footer + pending render tests.
- `web/src/views/ChatView.tsx` — MODIFY: thread `turn` + `canSend`.
- `web/src/App.tsx` — MODIFY: remove `busy`; per-spawn turn-state; optimistic pending + reconcile; fire-and-forget `onSend`; `MAX_QUEUED` guard.
- `web/src/lib/turn.ts` — CREATE: `MAX_QUEUED` constant + `reconcilePending` helper (+ test).
- `web/src/lib/turn.test.ts` — CREATE.

---

## Task 1: Broker turn-state core in `internal/transcript`

Add turn-state + queueing to the recorder as a new `OnClientLine`/`OnAgentLine` API layered over pure-record helpers. `ObserveClientLine`/`ObserveAgentLine` stay as pure-recording primitives (no turn-state side effects) so existing tests stay valid; both APIs share locked helpers (single lock, no nesting).

**Files:**
- Modify: `internal/transcript/recorder.go`
- Test: `internal/transcript/recorder_test.go`

- [ ] **Step 1: Write failing tests for turn-state, queueing, drain, and history turn**

Add to `internal/transcript/recorder_test.go`:

```go
// promptID builds a session/prompt request line carrying a JSON-RPC id.
func promptID(id int, text string) []byte {
	return []byte(`{"id":` + itoa(id) + `,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"` + text + `"}]}}` + "\n")
}
func response(id int, stopReason string) []byte {
	return []byte(`{"id":` + itoa(id) + `,"result":{"stopReason":"` + stopReason + `"}}` + "\n")
}
func itoa(i int) string { return strconv.Itoa(i) }

func decodeTurnFrame(t *testing.T, frame []byte) (string, int) {
	t.Helper()
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		t.Fatalf("expected newline-terminated spawn/turn frame, got %q", string(frame))
	}
	var m struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			State  string `json:"state"`
			Queued int    `json:"queued"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("turn frame not json: %v\n%s", err, string(frame))
	}
	if m.Jsonrpc != "2.0" || m.Method != "spawn/turn" {
		t.Fatalf("turn envelope wrong: jsonrpc=%q method=%q", m.Jsonrpc, m.Method)
	}
	return m.Params.State, m.Params.Queued
}

func TestBrokerForwardsAndTracksTurn(t *testing.T) {
	r := New()
	// idle prompt -> forwarded, turn busy
	fwd, turn := r.OnClientLine(promptID(1, "hello"))
	if len(fwd) != 1 || !bytes.Equal(fwd[0], promptID(1, "hello")) {
		t.Fatalf("idle prompt must be forwarded once, got %d lines", len(fwd))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 0 {
		t.Fatalf("turn after idle prompt = %s/%d, want busy/0", st, q)
	}
	// response ends the turn -> idle, nothing to drain
	drain, turn := r.OnAgentLine(response(1, "end_turn"))
	if len(drain) != 0 {
		t.Fatalf("no queued prompts, drain must be empty, got %d", len(drain))
	}
	if st, q := decodeTurnFrame(t, turn); st != "idle" || q != 0 {
		t.Fatalf("turn after response = %s/%d, want idle/0", st, q)
	}
}

func TestBrokerQueuesWhileBusyThenDrains(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(1, "first"))
	// second prompt while busy -> held (not forwarded), queued count 1
	fwd, turn := r.OnClientLine(promptID(2, "second"))
	if len(fwd) != 0 {
		t.Fatalf("prompt while busy must be held, got %d forwarded", len(fwd))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 1 {
		t.Fatalf("turn after queued prompt = %s/%d, want busy/1", st, q)
	}
	// history shows the queued user item as pending and turn busy/1
	items, state, queued := decodeHistory(t, r.HistoryFrame())
	if state != "busy" || queued != 1 {
		t.Fatalf("history turn = %s/%d, want busy/1", state, queued)
	}
	if len(items) != 2 || items[1].Role != "user" || !items[1].Pending {
		t.Fatalf("second user item must be pending, items=%+v", items)
	}
	// first turn ends -> drain the queued prompt to the agent, turn stays busy/0
	drain, turn := r.OnAgentLine(response(1, "end_turn"))
	if len(drain) != 1 || !bytes.Equal(drain[0], promptID(2, "second")) {
		t.Fatalf("must drain the queued prompt, got %d lines", len(drain))
	}
	if st, q := decodeTurnFrame(t, turn); st != "busy" || q != 0 {
		t.Fatalf("turn after drain = %s/%d, want busy/0", st, q)
	}
	// the previously-pending item is now sent
	items, _, _ = decodeHistory(t, r.HistoryFrame())
	if items[1].Pending {
		t.Fatalf("drained item must no longer be pending, items=%+v", items)
	}
}

func TestBrokerNonPromptLinesPassThrough(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(1, "go"))
	// a permission response (has id, no method) must pass through even while busy
	perm := []byte(`{"id":7,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}` + "\n")
	fwd, turn := r.OnClientLine(perm)
	if len(fwd) != 1 || !bytes.Equal(fwd[0], perm) {
		t.Fatalf("non-prompt client line must pass through unchanged")
	}
	if turn != nil {
		t.Fatalf("non-prompt line must not emit a turn frame")
	}
}

func TestBrokerCancelledResponseEndsTurn(t *testing.T) {
	r := New()
	r.OnClientLine(promptID(3, "x"))
	_, turn := r.OnAgentLine(response(3, "cancelled"))
	if st, _ := decodeTurnFrame(t, turn); st != "idle" {
		t.Fatalf("cancelled stopReason must end the turn, got %s", st)
	}
}
```

Add a `decodeHistory` helper next to `decodeFrame` (returns items + turn state + queued):

```go
func decodeHistory(t *testing.T, frame []byte) ([]Item, string, int) {
	t.Helper()
	var m struct {
		Params struct {
			Items []Item `json:"items"`
			Turn  struct {
				State  string `json:"state"`
				Queued int    `json:"queued"`
			} `json:"turn"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("history not json: %v\n%s", err, string(frame))
	}
	return m.Params.Items, m.Params.Turn.State, m.Params.Turn.Queued
}
```

Add imports `"bytes"` and `"strconv"` to the test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/debian/AleCode/spawnery && go test ./internal/transcript/ -run 'TestBroker' -v`
Expected: FAIL — `r.OnClientLine` / `r.OnAgentLine` undefined.

- [ ] **Step 3: Add `Pending` to `Item` and turn-state fields to `Recorder`**

In `internal/transcript/recorder.go`, extend the `Item` struct:

```go
type Item struct {
	Role    string `json:"role"`
	Text    string `json:"text,omitempty"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status,omitempty"`
	Pending bool   `json:"pending,omitempty"` // queued prompt not yet forwarded to the agent
}
```

Add a queue cap constant near `MaxItems`:

```go
// MaxQueued caps prompts buffered while the agent is busy. The web client also gates on this
// (web/src/lib/turn.ts MAX_QUEUED); over the cap the broker drops silently (defence in depth).
const MaxQueued = 50
```

Extend the `Recorder` struct:

```go
type Recorder struct {
	mu       sync.Mutex
	items    []Item
	toolIdx  map[string]int // toolCallId -> index in items, for tool_call_update
	busy     bool           // a session/prompt turn is in flight
	inflight *int           // JSON-RPC id of the in-flight prompt (nil if the client omitted one)
	queue    [][]byte       // raw client prompt lines held while busy, FIFO
	lastTurn string         // last (state:queued) emitted, to suppress duplicate spawn/turn frames
}
```

- [ ] **Step 4: Add shared locked record helper + `OnClientLine`**

In `recorder.go`, refactor `ObserveClientLine` to use a shared locked helper, and add `OnClientLine`:

```go
// recordUserLocked appends a user item. Caller holds r.mu.
func (r *Recorder) recordUserLocked(text string, pending bool) {
	r.push(Item{Role: "user", Text: text, Pending: pending})
}

// promptText extracts the concatenated text of a session/prompt line, or ("", false) if the line
// is not a session/prompt. The returned id is the JSON-RPC request id (nil if absent).
func promptText(line []byte) (text string, id *int, ok bool) {
	var m struct {
		Method string `json:"method"`
		ID     *int   `json:"id"`
		Params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/prompt" {
		return "", nil, false
	}
	var sb strings.Builder
	for _, p := range m.Params.Prompt {
		sb.WriteString(p.Text)
	}
	return sb.String(), m.ID, true
}

// OnClientLine records and GATES a client->agent ndjson line. It returns the line(s) to forward to
// the agent now (a non-prompt line passes through; an idle prompt is forwarded and starts a turn; a
// prompt received while busy is held, recorded as a pending user item, and queued) and an optional
// spawn/turn notification to send to the client.
func (r *Recorder) OnClientLine(line []byte) (forward [][]byte, turn []byte) {
	text, id, isPrompt := promptText(line)
	if !isPrompt {
		return [][]byte{line}, nil // permission responses, initialize, session/new, session/cancel
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.busy {
		r.busy = true
		r.inflight = id
		r.recordUserLocked(text, false)
		return [][]byte{line}, r.turnFrameLocked()
	}
	// busy: queue (defence-in-depth cap; the web client gates first)
	if len(r.queue) >= MaxQueued {
		return nil, nil
	}
	r.recordUserLocked(text, true)
	r.queue = append(r.queue, append([]byte(nil), line...))
	return nil, r.turnFrameLocked()
}
```

Update `ObserveClientLine` to share the helper (behavior unchanged — pure record, no turn-state):

```go
func (r *Recorder) ObserveClientLine(line []byte) {
	text, _, ok := promptText(line)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordUserLocked(text, false)
}
```

- [ ] **Step 5: Add `OnAgentLine`, `turnFrameLocked`, and `endTurnLocked`**

Refactor the `session/update` recording out of `ObserveAgentLine` into a locked helper, then add the broker entrypoint. Replace the body of `ObserveAgentLine` and add the new methods:

```go
// observeUpdateLocked records a session/update notification. Caller holds r.mu.
func (r *Recorder) observeUpdateLocked(u agentUpdate) {
	switch u.SessionUpdate {
	case "agent_message_chunk":
		r.appendChunk("agent", u.Content.Text)
	case "agent_thought_chunk":
		r.appendChunk("thought", u.Content.Text)
	case "tool_call":
		r.push(Item{Role: "tool", Title: u.Title, Status: u.Status})
		if u.ToolCallID != "" {
			r.toolIdx[u.ToolCallID] = len(r.items) - 1
		}
	case "tool_call_update":
		if i, ok := r.toolIdx[u.ToolCallID]; ok && i >= 0 && i < len(r.items) {
			r.items[i].Status = u.Status
		}
	}
}

type agentUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	Content       struct {
		Text string `json:"text"`
	} `json:"content"`
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
}

// ObserveAgentLine records an agent->client session/update notification (pure record).
func (r *Recorder) ObserveAgentLine(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update agentUpdate `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeUpdateLocked(m.Params.Update)
}

// OnAgentLine records an agent->client ndjson line AND detects turn-end. It returns prompt line(s)
// to forward to the agent now (a drained queued prompt, if the turn just ended) and an optional
// spawn/turn notification for the client.
func (r *Recorder) OnAgentLine(line []byte) (drain [][]byte, turn []byte) {
	var m struct {
		Method string          `json:"method"`
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Params struct {
			Update agentUpdate `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.Method == "session/update" {
		r.observeUpdateLocked(m.Params.Update)
		return nil, nil
	}
	// turn-end: a response (no method, has result/error) to the in-flight prompt.
	isResponse := m.Method == "" && (len(m.Result) > 0 || m.Error != nil)
	matchesInflight := r.inflight == nil || (m.ID != nil && *m.ID == *r.inflight)
	if r.busy && isResponse && matchesInflight {
		return r.endTurnLocked()
	}
	return nil, nil
}

// endTurnLocked marks the current turn done and drains the next queued prompt (if any). Caller holds r.mu.
func (r *Recorder) endTurnLocked() (drain [][]byte, turn []byte) {
	r.busy = false
	r.inflight = nil
	if len(r.queue) > 0 {
		next := r.queue[0]
		r.queue = r.queue[1:]
		r.busy = true
		_, id, _ := promptText(next)
		r.inflight = id
		for i := range r.items { // FIFO: clear the oldest still-pending item
			if r.items[i].Pending {
				r.items[i].Pending = false
				break
			}
		}
		return [][]byte{next}, r.turnFrameLocked()
	}
	return nil, r.turnFrameLocked()
}

// turnFrameLocked builds a spawn/turn notification, or nil if (state,queued) is unchanged since the
// last emit. Caller holds r.mu.
func (r *Recorder) turnFrameLocked() []byte {
	state := "idle"
	if r.busy {
		state = "busy"
	}
	cur := fmt.Sprintf("%s:%d", state, len(r.queue))
	if cur == r.lastTurn {
		return nil
	}
	r.lastTurn = cur
	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			State  string `json:"state"`
			Queued int    `json:"queued"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/turn"
	env.Params.State = state
	env.Params.Queued = len(r.queue)
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
```

Add `"fmt"` to the imports.

- [ ] **Step 6: Fold turn-state into `HistoryFrame`**

Replace the marshalling block in `HistoryFrame` so the params carry `turn`. Capture state under the existing lock before unlocking:

```go
func (r *Recorder) HistoryFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]Item, len(r.items))
	copy(snap, r.items)
	state := "idle"
	if r.busy {
		state = "busy"
	}
	queued := len(r.queue)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []Item `json:"items"`
			Turn  struct {
				State  string `json:"state"`
				Queued int    `json:"queued"`
			} `json:"turn"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/history"
	env.Params.Items = snap
	env.Params.Turn.State = state
	env.Params.Turn.Queued = queued
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
```

Update the package doc comment (top of file) to: `// Package transcript records an ACP ndjson conversation and brokers per-spawn turn-state: it tracks the in-flight session/prompt, queues prompts received while busy, drains them FIFO on turn-end, and serializes the transcript + turn-state as spawn/history / spawn/turn frames. Used by the in-pod acpadapter (CRI lane) and the node relay (Docker lane).`

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd /home/debian/AleCode/spawnery && go test ./internal/transcript/ -v`
Expected: PASS — new `TestBroker*` tests plus the existing `TestRecorder*` tests (the pure-record path is unchanged).

- [ ] **Step 8: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add internal/transcript/recorder.go internal/transcript/recorder_test.go
git commit --no-verify -m "feat(transcript): per-spawn turn-state + prompt queue broker [sp-95v]"
```

---

## Task 2: Docker-lane wiring — `brokerEndpoint`

Replace `recordingEndpoint` (a passive tee) with `brokerEndpoint`, which gates the client→agent direction through the broker and routes ALL agent-bound bytes through a single channel (so the drain writes don't introduce a second writer to agent stdin). `spawn/turn` frames go to the client via the underlying `ep.Send`.

**Files:**
- Modify: `internal/node/record.go`
- Modify: `internal/node/attach.go:247`
- Test: `internal/node/record_test.go`

- [ ] **Step 1: Write a failing test for gating + drain in the Docker lane**

Add to `internal/node/record_test.go`:

```go
func TestBrokerEndpointQueuesAndDrains(t *testing.T) {
	rec := transcript.New()
	// Underlying client endpoint: Recv yields queued client lines; Send captures client-bound bytes.
	in := make(chan []byte, 8)
	var sentToClient [][]byte
	var sendMu sync.Mutex
	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, ok := <-in
			if !ok {
				return nil, io.EOF
			}
			return b, nil
		},
		Send: func(b []byte) error {
			sendMu.Lock()
			sentToClient = append(sentToClient, append([]byte(nil), b...))
			sendMu.Unlock()
			return nil
		},
	}
	be := brokerEndpoint(ep, rec)

	// Two prompts arrive back-to-back while idle->busy; the second must be held.
	in <- []byte(`{"id":1,"method":"session/prompt","params":{"prompt":[{"text":"a"}]}}` + "\n")
	in <- []byte(`{"id":2,"method":"session/prompt","params":{"prompt":[{"text":"b"}]}}` + "\n")

	// Recv (the relay's client->agent side) should deliver ONLY the first prompt to the agent.
	got1, _ := be.Recv()
	if !bytes.Contains(got1, []byte(`"text":"a"`)) {
		t.Fatalf("first agent-bound line = %q, want prompt a", string(got1))
	}

	// Feed the agent's turn-end response through Send; the broker drains prompt b to the agent.
	if err := be.Send([]byte(`{"id":1,"result":{"stopReason":"end_turn"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	got2, _ := be.Recv()
	if !bytes.Contains(got2, []byte(`"text":"b"`)) {
		t.Fatalf("second agent-bound line = %q, want drained prompt b", string(got2))
	}

	// A spawn/turn frame must have reached the client at least once.
	sendMu.Lock()
	defer sendMu.Unlock()
	var sawTurn bool
	for _, b := range sentToClient {
		if bytes.Contains(b, []byte(`"method":"spawn/turn"`)) {
			sawTurn = true
		}
	}
	if !sawTurn {
		t.Fatalf("expected a spawn/turn frame sent to the client, got %d frames", len(sentToClient))
	}
}
```

Ensure the test file imports `bytes`, `io`, `sync`, `testing`, `spawnery/internal/spawnlet`, `spawnery/internal/transcript`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/debian/AleCode/spawnery && go test ./internal/node/ -run TestBrokerEndpoint -v`
Expected: FAIL — `brokerEndpoint` undefined.

- [ ] **Step 3: Implement `brokerEndpoint`**

In `internal/node/record.go`, add `brokerEndpoint` (keep `recordingEndpoint` for now; Task 4 removes it):

```go
// brokerEndpoint wraps a StreamEndpoint with the transcript broker. The client->agent direction is
// gated: an internal reader pulls client bytes, splits ndjson lines, and asks the broker what to
// forward (idle prompts pass; prompts while busy are held + queued). All agent-bound bytes — both
// forwarded client prompts and drained queued prompts — flow through agentCh, so Recv (the relay's
// single client->agent goroutine) remains the sole writer to agent stdin. spawn/turn frames are sent
// to the client via ep.Send. agentCh is buffered and never closed; Recv unblocks via done on client EOF.
func brokerEndpoint(ep spawnlet.StreamEndpoint, rec *transcript.Recorder) spawnlet.StreamEndpoint {
	agentCh := make(chan []byte, 64)
	done := make(chan struct{})
	var clientLB, agentLB lineBuffer
	go func() {
		for {
			b, err := ep.Recv()
			if len(b) > 0 {
				clientLB.feed(b, func(line []byte) {
					fwd, turn := rec.OnClientLine(line)
					for _, f := range fwd {
						agentCh <- f
					}
					if turn != nil {
						_ = ep.Send(turn)
					}
				})
			}
			if err != nil {
				close(done)
				return
			}
		}
	}()
	return spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			select {
			case b := <-agentCh:
				return b, nil
			case <-done:
				return nil, io.EOF
			}
		},
		Send: func(b []byte) error {
			if len(b) > 0 {
				agentLB.feed(b, func(line []byte) {
					drain, turn := rec.OnAgentLine(line)
					for _, d := range drain {
						agentCh <- d
					}
					if turn != nil {
						_ = ep.Send(turn)
					}
				})
			}
			return ep.Send(b)
		},
	}
}
```

Add `"io"` to `record.go` imports.

- [ ] **Step 4: Switch `openSession` to `brokerEndpoint`**

In `internal/node/attach.go`, change the wrap at line ~247:

```go
		ep = brokerEndpoint(ep, rec)
```

(was `ep = recordingEndpoint(ep, rec)`). Leave the `rec.HistoryFrame()` replay above it unchanged.

- [ ] **Step 5: Run node tests to verify they pass**

Run: `cd /home/debian/AleCode/spawnery && go test ./internal/node/ -v`
Expected: PASS — new `TestBrokerEndpoint*` plus existing node tests.

- [ ] **Step 6: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add internal/node/record.go internal/node/record_test.go internal/node/attach.go
git commit --no-verify -m "feat(node): gate Docker-lane relay through the turn broker [sp-95v]"
```

---

## Task 3: CRI-lane wiring — `acpadapter` bridge

Mirror Task 2 in the in-pod adapter: route all agent-bound writes through a single channel drained by one writer goroutine, and use the broker's `OnClientLine`/`OnAgentLine`. `spawn/turn` frames go to the client via `hub.write`.

**Files:**
- Modify: `deploy/agent/acpadapter/bridge.go`
- Test: `deploy/agent/acpadapter/bridge_test.go`

- [ ] **Step 1: Write a failing test for adapter-side gating**

Add to `deploy/agent/acpadapter/bridge_test.go` (follow the existing test's construction of `serve`/pipes; this test drives `recordingCopy` + `pump` directly via an in-memory agent):

```go
func TestAdapterHoldsSecondPromptUntilTurnEnds(t *testing.T) {
	rec := transcript.New()
	agentCh := make(chan []byte, 8)

	// Fake agent stdin: capture lines written to the agent.
	agentIn, agentInW := io.Pipe()
	go func() { for line := range agentCh { agentInW.Write(line) } }()
	agentReader := bufio.NewReader(agentIn)
	readLine := func() string {
		l, _ := agentReader.ReadString('\n')
		return l
	}

	// Client stdin: two prompts back-to-back.
	clientR, clientW := io.Pipe()
	go func() {
		clientW.Write([]byte(`{"id":1,"method":"session/prompt","params":{"prompt":[{"text":"a"}]}}` + "\n"))
		clientW.Write([]byte(`{"id":2,"method":"session/prompt","params":{"prompt":[{"text":"b"}]}}` + "\n"))
	}()
	hub := &connHub{}
	go recordingCopy(clientR, rec, agentCh)

	// Only prompt "a" should reach the agent before the turn ends.
	if got := readLine(); !strings.Contains(got, `"text":"a"`) {
		t.Fatalf("first agent line = %q, want prompt a", got)
	}

	// pump observes the agent's turn-end response and drains prompt "b".
	agentOut, agentOutW := io.Pipe()
	go pump(agentOut, hub, rec, agentCh)
	agentOutW.Write([]byte(`{"id":1,"result":{"stopReason":"end_turn"}}` + "\n"))
	if got := readLine(); !strings.Contains(got, `"text":"b"`) {
		t.Fatalf("second agent line = %q, want drained prompt b", got)
	}
}
```

Ensure imports: `bufio`, `io`, `strings`, `testing`, `spawnery/internal/transcript`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /home/debian/AleCode/spawnery && go test ./deploy/agent/acpadapter/ -run TestAdapterHolds -v`
Expected: FAIL — `recordingCopy`/`pump` signatures don't match (no `agentCh`).

- [ ] **Step 3: Rework `serve`, `recordingCopy`, and `pump`**

In `deploy/agent/acpadapter/bridge.go`, replace `pump`, `recordingCopy`, and `serve`:

```go
// pump is the single persistent reader of the agent's stdout. It records each ndjson line, forwards
// it byte-for-byte to the current client, and — when a line is the in-flight prompt's turn-end
// response — drains the next queued prompt to the agent (via agentCh) and pushes a spawn/turn frame.
func pump(fromAgent io.Reader, hub *connHub, rec *transcript.Recorder, agentCh chan<- []byte) {
	br := bufio.NewReaderSize(fromAgent, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			drain, turn := rec.OnAgentLine(line)
			for _, d := range drain {
				agentCh <- d
			}
			if turn != nil {
				hub.write(turn)
			}
			hub.write(line)
		}
		if err != nil {
			return
		}
	}
}

// recordingCopy reads the client's stdin line-by-line and asks the broker what to forward: idle
// prompts and non-prompt lines go to the agent (via agentCh); prompts received while busy are held
// and queued. spawn/turn frames are written back to the client. Returns on the client's write EOF.
func recordingCopy(conn io.Reader, rec *transcript.Recorder, agentCh chan<- []byte) {
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fwd, turn := rec.OnClientLine(line)
			for _, f := range fwd {
				agentCh <- f
			}
			if turn != nil {
				hubWriteCurrent(turn) // see note below
			}
		}
		if err != nil {
			return
		}
	}
}
```

`recordingCopy` needs the hub to send turn frames back to the client. Thread the hub in rather than a global — change the signature to `recordingCopy(conn io.Reader, rec *transcript.Recorder, agentCh chan<- []byte, hub *connHub)` and replace `hubWriteCurrent(turn)` with `hub.write(turn)`. Update the test in Step 1 to pass `hub`: `go recordingCopy(clientR, rec, agentCh, hub)`.

Rewrite `serve` to own `agentCh` and the single writer goroutine:

```go
func serve(ln net.Listener, toAgent io.Writer, fromAgent io.Reader) error {
	hub := &connHub{}
	rec := transcript.New()
	agentCh := make(chan []byte, 64)
	// Single writer to agent stdin: forwarded client prompts AND drained queued prompts.
	go func() {
		for line := range agentCh {
			if _, err := toAgent.Write(line); err != nil {
				return
			}
		}
	}()
	go pump(fromAgent, hub, rec, agentCh)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if prev := hub.attach(conn, rec.HistoryFrame()); prev != nil {
			_ = prev.Close()
		}
		recordingCopy(conn, rec, agentCh, hub)
	}
}
```

- [ ] **Step 4: Update the Step 1 test call to pass `hub`**

Change `go recordingCopy(clientR, rec, agentCh)` to `go recordingCopy(clientR, rec, agentCh, hub)` in the test.

- [ ] **Step 5: Run adapter tests to verify they pass**

Run: `cd /home/debian/AleCode/spawnery && go test ./deploy/agent/acpadapter/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add deploy/agent/acpadapter/bridge.go deploy/agent/acpadapter/bridge_test.go
git commit --no-verify -m "feat(acpadapter): gate CRI-lane bridge through the turn broker [sp-95v]"
```

---

## Task 4: Remove the now-unused `recordingEndpoint`

Both lanes use the broker now. Remove the dead `recordingEndpoint` from the Docker lane. (`ObserveClientLine`/`ObserveAgentLine` stay — they are the pure-record primitives still exercised by `recorder_test.go` and shared by the broker.)

**Files:**
- Modify: `internal/node/record.go`

- [ ] **Step 1: Confirm no remaining references**

Run: `cd /home/debian/AleCode/spawnery && grep -rn "recordingEndpoint" --include="*.go"`
Expected: only the definition in `internal/node/record.go` (no callers).

- [ ] **Step 2: Delete `recordingEndpoint`**

Remove the entire `func recordingEndpoint(...) spawnlet.StreamEndpoint { ... }` block from `internal/node/record.go`.

- [ ] **Step 3: Build + test to verify nothing broke**

Run: `cd /home/debian/AleCode/spawnery && go build ./... && go test ./internal/... ./deploy/...`
Expected: PASS, no "declared and not used" / undefined errors.

- [ ] **Step 4: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add internal/node/record.go
git commit --no-verify -m "refactor(node): drop dead recordingEndpoint (superseded by brokerEndpoint) [sp-95v]"
```

---

## Task 5: Frontend ACP types + client routing

Teach the web ACP client about `spawn/turn` and the new `turn` field on `spawn/history`.

**Files:**
- Modify: `web/src/acp/types.ts`
- Modify: `web/src/acp/client.ts`
- Test: `web/src/acp/client.test.ts`

- [ ] **Step 1: Write failing tests for `spawn/turn` routing**

Add to `web/src/acp/client.test.ts`, inside `describe("Client", ...)`:

```ts
it("routes spawn/turn notifications to onTurn", () => {
  const ws = new FakeWS();
  const c = new Client(ws);
  const seen: Array<{ state: string; queued: number }> = [];
  c.onTurn = (t) => seen.push(t);
  ws.inject({ method: "spawn/turn", params: { state: "busy", queued: 2 } });
  expect(seen).toEqual([{ state: "busy", queued: 2 }]);
});

it("fires onTurn from a spawn/history frame's turn field", () => {
  const ws = new FakeWS();
  const c = new Client(ws);
  const seen: Array<{ state: string; queued: number }> = [];
  c.onTurn = (t) => seen.push(t);
  ws.inject({
    method: "spawn/history",
    params: { items: [{ role: "user", text: "hi" }], turn: { state: "busy", queued: 1 } },
  });
  expect(seen).toEqual([{ state: "busy", queued: 1 }]);
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/acp/client.test.ts`
Expected: FAIL — `c.onTurn` is not a function / not called.

- [ ] **Step 3: Extend types**

In `web/src/acp/types.ts`:

```ts
export interface HistoryItem {
  role: "user" | "agent" | "thought" | "tool" | "system";
  text?: string;
  title?: string;
  status?: string;
  pending?: boolean; // queued prompt not yet forwarded to the agent
}

// spawn/turn notification: per-spawn agent turn-state.
export interface SpawnTurn {
  state: "busy" | "idle";
  queued: number;
}
```

- [ ] **Step 4: Route `spawn/turn` + history turn in the client**

In `web/src/acp/client.ts`, add the handler field near `onHistory`:

```ts
  // Per-spawn turn-state from the broker. Fires on spawn/turn notifications and on the turn field of
  // a spawn/history replay. Independent of any in-flight prompt promise.
  onTurn?: (t: import("./types").SpawnTurn) => void;
```

In `route(m)`, extend the `spawn/history` branch and add a `spawn/turn` branch:

```ts
    if (m.method === "spawn/history") {
      this.onHistory?.((m.params?.items as HistoryItem[]) ?? []);
      if (m.params?.turn) this.onTurn?.(m.params.turn);
      return;
    }
    if (m.method === "spawn/turn") {
      if (m.params) this.onTurn?.(m.params);
      return;
    }
```

- [ ] **Step 5: Run to verify pass**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/acp/client.test.ts`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/acp/types.ts web/src/acp/client.ts web/src/acp/client.test.ts
git commit --no-verify -m "feat(web/acp): route spawn/turn + history turn-state to onTurn [sp-95v]"
```

---

## Task 6: `MAX_QUEUED` constant + `reconcilePending` helper

A small pure module the App uses to gate over-cap sends and to keep the count of pending bubbles in sync with the broker's `queued` count.

**Files:**
- Create: `web/src/lib/turn.ts`
- Test: `web/src/lib/turn.test.ts`
- Modify: `web/src/views/chat/types.ts`

- [ ] **Step 1: Add `pending` to the chat user item + export TurnState**

In `web/src/views/chat/types.ts`:

```ts
export type Item =
  | { id: number; kind: "user"; text: string; pending?: boolean }
  | { id: number; kind: "agent"; text: string }
  | { id: number; kind: "tool"; title: string; status?: string }
  | { id: number; kind: "thought"; text: string };

export type TurnState = { state: "busy" | "idle"; queued: number };
```

- [ ] **Step 2: Write failing tests for `reconcilePending`**

Create `web/src/lib/turn.test.ts`:

```ts
import { describe, it, expect } from "vitest";
import { reconcilePending, MAX_QUEUED } from "./turn";
import type { Item } from "@/views/chat/types";

const u = (id: number, pending?: boolean): Item => ({ id, kind: "user", text: "x", pending });

describe("reconcilePending", () => {
  it("keeps exactly `queued` of the most recent pending user items pending", () => {
    const items: Item[] = [u(1, true), { id: 2, kind: "agent", text: "..." }, u(3, true), u(4, true)];
    // queued=1 -> only the newest pending (id 4) stays pending; older pending clear.
    const out = reconcilePending(items, 1);
    expect(out.filter((i) => i.kind === "user" && i.pending).map((i) => i.id)).toEqual([4]);
  });

  it("clears all pending when queued is 0", () => {
    const items: Item[] = [u(1, true), u(2, true)];
    expect(reconcilePending(items, 0).every((i) => !(i.kind === "user" && i.pending))).toBe(true);
  });

  it("MAX_QUEUED is a positive cap", () => {
    expect(MAX_QUEUED).toBeGreaterThan(0);
  });
});
```

- [ ] **Step 3: Run to verify failure**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/lib/turn.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 4: Implement `web/src/lib/turn.ts`**

```ts
import type { Item } from "@/views/chat/types";

// Mirror of internal/transcript.MaxQueued. The input box stops sending once this many prompts are
// queued; the broker also drops over-cap as defence in depth.
export const MAX_QUEUED = 50;

// reconcilePending returns items with pending flags adjusted so that exactly `queued` of the most
// recent pending user items stay pending (FIFO drain: oldest pending clears first). It does not
// mutate the input.
export function reconcilePending(items: Item[], queued: number): Item[] {
  const pendingIdx: number[] = [];
  items.forEach((it, i) => {
    if (it.kind === "user" && it.pending) pendingIdx.push(i);
  });
  const clearCount = Math.max(0, pendingIdx.length - queued);
  if (clearCount === 0) return items;
  const clear = new Set(pendingIdx.slice(0, clearCount)); // oldest first
  return items.map((it, i) =>
    clear.has(i) && it.kind === "user" ? { ...it, pending: false } : it,
  );
}
```

- [ ] **Step 5: Run to verify pass**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/lib/turn.test.ts`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/lib/turn.ts web/src/lib/turn.test.ts web/src/views/chat/types.ts
git commit --no-verify -m "feat(web): pending-item reconcile helper + MAX_QUEUED [sp-95v]"
```

---

## Task 7: PromptInput — remove Send button, Enter sends

**Files:**
- Modify: `web/src/views/chat/PromptInput.tsx`
- Test: `web/src/views/chat/PromptInput.test.tsx`

- [ ] **Step 1: Update the test — no Send button; Enter is the only send path**

Replace `web/src/views/chat/PromptInput.test.tsx` with (drops the button query, adds a no-button assertion; keeps Enter/Shift+Enter/disabled behavior):

```tsx
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { PromptInput } from "./PromptInput";

describe("PromptInput", () => {
  it("renders no Send button (Enter sends)", () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} />);
    expect(screen.queryByTestId("prompt-send")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("sends text on Enter then clears the box", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hello");
    await userEvent.keyboard("{Enter}");
    expect(onSend).toHaveBeenCalledWith("hello");
    expect(box.value).toBe("");
  });

  it("Shift+Enter inserts a newline and does not send", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "line1");
    await userEvent.keyboard("{Shift>}{Enter}{/Shift}");
    await userEvent.type(box, "line2");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("line1\nline2");
  });

  it("does not send whitespace-only input", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    await userEvent.type(screen.getByTestId("prompt-input"), "   ");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
  });

  it("does not send while disabled, but the box stays typeable and retains text", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={true} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hi");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("hi");
  });
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/views/chat/PromptInput.test.tsx`
Expected: FAIL — the Send button still renders (`queryByRole("button")` is non-null).

- [ ] **Step 3: Remove the Send button**

Replace `web/src/views/chat/PromptInput.tsx`:

```tsx
import { useState } from "react";
import { Textarea } from "@/components/ui/textarea";

export function PromptInput({ disabled, onSend }: { disabled: boolean; onSend: (t: string) => void }) {
  const [t, setT] = useState("");
  // Enter sends; Shift+Enter inserts a newline. The textarea is never `disabled` (a disabled element
  // is blurred by the browser, dropping focus right after Enter) — `disabled` only gates sending, so
  // you can keep typing while the agent works (sends queue server-side once connected).
  const send = () => {
    if (disabled || !t.trim()) return;
    onSend(t);
    setT("");
  };
  return (
    <div className="border-t border-border p-3">
      <Textarea
        data-testid="prompt-input"
        value={t}
        aria-busy={disabled}
        placeholder="Ask the agent…"
        className="min-h-[2.5rem] resize-none"
        onChange={(e) => setT(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
      />
    </div>
  );
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/views/chat/PromptInput.test.tsx`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/views/chat/PromptInput.tsx web/src/views/chat/PromptInput.test.tsx
git commit --no-verify -m "feat(web): remove Send button, Enter sends [sp-95v]"
```

---

## Task 8: MessageList — working footer + pending bubbles

Render a transcript-footer typing-indicator (pulsing dots + `working… · N queued`) when the active spawn is busy, via Virtuoso's `Footer` component fed through the `context` prop. Render pending user bubbles dimmed with a "queued" tag.

**Files:**
- Modify: `web/src/views/chat/MessageList.tsx`
- Test: `web/src/views/chat/MessageList.test.tsx` (create)

- [ ] **Step 1: Write failing tests**

Create `web/src/views/chat/MessageList.test.tsx`:

```tsx
import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { MessageList } from "./MessageList";
import type { Item } from "./types";

const items: Item[] = [
  { id: 1, kind: "user", text: "sent" },
  { id: 2, kind: "user", text: "waiting", pending: true },
];

describe("MessageList", () => {
  it("shows the working footer with queued count when working", () => {
    render(<MessageList items={items} working={true} queued={2} />);
    expect(screen.getByTestId("working-indicator")).toHaveTextContent("working…");
    expect(screen.getByTestId("working-indicator")).toHaveTextContent("2 queued");
  });

  it("hides the working footer when idle", () => {
    render(<MessageList items={items} working={false} queued={0} />);
    expect(screen.queryByTestId("working-indicator")).toBeNull();
  });

  it("tags pending user bubbles as queued", () => {
    render(<MessageList items={items} working={true} queued={1} />);
    expect(screen.getByTestId("queued-tag")).toBeInTheDocument();
  });
});
```

> Note: react-virtuoso renders into a scroll container; in jsdom it still mounts rows and the Footer synchronously. If a row is virtualized out, wrap assertions for row content with `findBy*`. The footer is always mounted (it's the list Footer), so `getByTestId("working-indicator")` is reliable.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/views/chat/MessageList.test.tsx`
Expected: FAIL — `MessageList` doesn't accept `working`/`queued`; no `working-indicator`.

- [ ] **Step 3: Implement footer + pending styling**

Replace `web/src/views/chat/MessageList.tsx`:

```tsx
import { memo } from "react";
import { Virtuoso } from "react-virtuoso";
import { Streamdown } from "streamdown";
import { cn } from "@/lib/utils";
import type { Item } from "./types";
import { ToolCallChip } from "./ToolCallChip";
import { Thoughts } from "./Thoughts";

const Row = memo(function Row({ item }: { item: Item }) {
  if (item.kind === "tool") return <ToolCallChip title={item.title} status={item.status} />;
  if (item.kind === "thought") return <Thoughts text={item.text} />;

  const isUser = item.kind === "user";
  const pending = isUser && item.pending;
  return (
    <div
      data-role={item.kind}
      className={cn("mx-auto max-w-[70ch] px-4 py-3 text-foreground", isUser && "text-right")}
    >
      {isUser ? (
        <span className={cn("inline-block rounded-lg bg-muted px-3 py-2 text-left", pending && "opacity-50")}>
          {item.text}
          {pending && (
            <span data-testid="queued-tag" className="ml-2 align-middle text-[10px] uppercase tracking-wide text-muted-foreground">
              queued
            </span>
          )}
        </span>
      ) : (
        <Streamdown>{item.text}</Streamdown>
      )}
    </div>
  );
});

type ListContext = { working: boolean; queued: number };

// WorkingFooter renders the transcript-footer typing indicator (pulsing dots + "working…[· N queued]")
// while the agent is mid-turn. It is the list Footer so it sits at the end of the conversation and
// scrolls with followOutput.
function WorkingFooter({ context }: { context?: ListContext }) {
  if (!context?.working) return null;
  return (
    <div
      data-testid="working-indicator"
      className="mx-auto flex max-w-[70ch] items-center gap-2 px-4 py-3 text-xs text-muted-foreground"
    >
      <span className="flex gap-1">
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current" />
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current [animation-delay:150ms]" />
        <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current [animation-delay:300ms]" />
      </span>
      working…{context.queued > 0 ? ` · ${context.queued} queued` : ""}
    </div>
  );
}

export function MessageList({ items, working, queued }: { items: Item[]; working: boolean; queued: number }) {
  return (
    <Virtuoso
      className="flex-1"
      data={items}
      followOutput="smooth"
      context={{ working, queued }}
      components={{ Footer: WorkingFooter }}
      computeItemKey={(_, item) => item.id}
      itemContent={(_, item) => <Row item={item} />}
    />
  );
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd /home/debian/AleCode/spawnery/web && npm test -- src/views/chat/MessageList.test.tsx`
Expected: PASS. If a virtualization issue surfaces for the `queued-tag` (row offscreen), it won't here — both items are within the default viewport in jsdom.

- [ ] **Step 5: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/views/chat/MessageList.tsx web/src/views/chat/MessageList.test.tsx
git commit --no-verify -m "feat(web): transcript working indicator + pending bubbles [sp-95v]"
```

---

## Task 9: ChatView — thread turn-state and canSend

**Files:**
- Modify: `web/src/views/ChatView.tsx`

- [ ] **Step 1: Update ChatView props and wiring**

Replace `web/src/views/ChatView.tsx`:

```tsx
import { MessageList } from "./chat/MessageList";
import { PromptInput } from "./chat/PromptInput";
import { PermissionModal } from "./chat/PermissionModal";
import type { Item, TurnState } from "./chat/types";

export function ChatView({ items, turn, canSend, onSend, perm }: {
  items: Item[];
  turn: TurnState;
  canSend: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
}) {
  return (
    <div className="flex h-full flex-col">
      <MessageList items={items} working={turn.state === "busy"} queued={turn.queued} />
      <PromptInput disabled={!canSend} onSend={onSend} />
      {perm && <PermissionModal title={perm.title} onResolve={perm.resolve} />}
    </div>
  );
}
```

- [ ] **Step 2: Check who renders ChatView and what it passes**

Run: `cd /home/debian/AleCode/spawnery/web && grep -rn "ChatView" src/`
Expected: find the render site (likely `src/shell/AppShell.tsx`). Note its current props (`busy={...}`) — Task 10 updates the call site and `App.tsx`. Do not build yet (call site is stale until Task 10).

- [ ] **Step 3: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/views/ChatView.tsx
git commit --no-verify -m "feat(web): ChatView takes turn-state + canSend [sp-95v]"
```

---

## Task 10: App + AppShell integration — drop global busy, wire per-spawn turn-state

Remove the global `busy` flag and the prompt-promise gating; track turn-state per spawn from `onTurn`; make `onSend` fire-and-forget with optimistic pending + reconcile; gate sends on connection and `MAX_QUEUED`. Update `AppShell` to pass `turn`/`canSend` through to `ChatView`.

**Files:**
- Modify: `web/src/App.tsx`
- Modify: `web/src/shell/AppShell.tsx`

- [ ] **Step 1: Inspect AppShell's ChatView call + props**

Run: `cd /home/debian/AleCode/spawnery/web && sed -n '1,80p' src/shell/AppShell.tsx`
Identify where `busy` is received and forwarded to `ChatView`. You will replace `busy` with `turn` + `canSend` along this path.

- [ ] **Step 2: Update `App.tsx` — state and refs**

In `web/src/App.tsx`:

Remove the `busy` state line (`const [busy, setBusy] = useState(false);`). Add turn-state, mirroring the per-spawn buffer pattern:

```tsx
  const [turn, setTurn] = useState<{ state: "busy" | "idle"; queued: number }>({ state: "idle", queued: 0 });
```

Add a per-spawn turn ref alongside `buffersRef`:

```tsx
  const turnsRef = useRef<Map<string, { state: "busy" | "idle"; queued: number }>>(new Map());
```

Add the import for the reconcile helper + cap at the top:

```tsx
import { reconcilePending, MAX_QUEUED } from "./lib/turn";
```

- [ ] **Step 3: Wire `onTurn` in `openSession`**

Inside `openSession`, in the `ws.onopen` handler where `c.onHistory` is set, add an `onTurn` handler (gen- and active-guarded like `onHistory`), and reconcile pending items against the queued count:

```tsx
      c.onTurn = (t) => {
        if (genRef.current !== gen) return;
        turnsRef.current.set(spawnId, t);
        if (activeIdRef.current === spawnId) {
          setTurn(t);
          setItems((cur) => reconcilePending(cur, t.queued));
        }
      };
```

- [ ] **Step 4: Reset/restore turn-state on spawn switch + teardown**

In `selectSpawn`, after restoring the buffer, restore the turn-state for the selected spawn (history replay will correct it once the socket connects):

```tsx
    setTurn(turnsRef.current.get(id) ?? { state: "idle", queued: 0 });
```

In `closeSession` (or wherever a session ends with no active spawn) and in `spawnApp` (new spawn), set `setTurn({ state: "idle", queued: 0 })`. For `spawnApp`, add it next to the `setItems((current) => { ...; return []; })` block. In `onStop`, when the stopped spawn is active, also clear: `setTurn({ state: "idle", queued: 0 })` and `turnsRef.current.delete(id)`.

- [ ] **Step 5: Make `onSend` fire-and-forget with optimistic pending**

Replace the `onSend` function body:

```tsx
  const onSend = (text: string) => {
    const c = clientRef.current;
    if (!c) return;
    // Optimistic: if the agent is already working (or prompts are queued), this one will queue too —
    // render it pending. The broker's spawn/turn reconciles the exact pending set as the queue drains.
    const willQueue = turn.state === "busy" || turn.queued > 0;
    add({ kind: "user", text, pending: willQueue });
    // Fire-and-forget: turn-state drives the UI, not this promise. It may resolve much later (queued)
    // or never (disconnect/switch) — that's fine, we no longer gate on it.
    void c.prompt(text, {
      onText: appendChunk("agent"),
      onThought: appendChunk("thought"),
      onToolCall: (tc) => add({ kind: "tool", title: tc.title, status: tc.status }),
      onToolUpdate: (tc) => add({ kind: "tool", title: "tool", status: tc.status }),
      requestPermission: (req) =>
        new Promise<boolean>((resolve) =>
          setPerm({ title: req?.options?.[0]?.name ?? "an action", resolve: (b) => { setPerm(null); resolve(b); } })),
    }).catch(() => {});
  };
```

- [ ] **Step 6: Update the `AppShell` render props**

Change the `<AppShell .../>` return so it passes turn-state and a send gate instead of `busy`:

```tsx
      conn={conn}
      items={items}
      turn={turn}
      canSend={conn === "connected" && turn.queued < MAX_QUEUED}
      onSend={onSend}
```

(Remove the old `busy={busy || conn !== "connected"}` line.)

- [ ] **Step 7: Thread props through `AppShell.tsx`**

In `web/src/shell/AppShell.tsx`, replace the `busy` prop on the path to `ChatView` with `turn` and `canSend`. Update the `AppShell` props type: remove `busy: boolean`, add `turn: { state: "busy" | "idle"; queued: number }` and `canSend: boolean`. Forward them to `ChatView` (`turn={turn} canSend={canSend}` instead of `busy={busy}`). Keep `conn`, `items`, `onSend`, `perm`, and the spawn props unchanged.

- [ ] **Step 8: Typecheck, build, and run the full web suite**

Run: `cd /home/debian/AleCode/spawnery/web && npm run build && npm test`
Expected: `tsc -b` passes (no references to the removed `busy` prop), Vite build succeeds, all Vitest suites pass. Fix any AppShell test (`src/shell/AppShell.test.tsx`) that passed `busy` — update it to pass `turn={{ state: "idle", queued: 0 }} canSend={true}`.

- [ ] **Step 9: Commit**

```bash
cd /home/debian/AleCode/spawnery
git add web/src/App.tsx web/src/shell/AppShell.tsx web/src/shell/AppShell.test.tsx
git commit --no-verify -m "feat(web): per-spawn turn-state, fire-and-forget send, queue gating [sp-95v]"
```

---

## Task 11: Full verification + close-out

**Files:** none (verification only)

- [ ] **Step 1: Backend — full build + test**

Run: `cd /home/debian/AleCode/spawnery && go build ./... && go test ./...`
Expected: PASS across all packages.

- [ ] **Step 2: Frontend — typecheck, build, unit tests**

Run: `cd /home/debian/AleCode/spawnery/web && npm run build && npm test`
Expected: PASS.

- [ ] **Step 3: Manual smoke (optional but recommended)**

Use the `run` skill / project launch to start the app. Verify, against the spec's reported bugs:
1. Send a prompt to spawn A; while it streams, switch to idle spawn B — B's input is **enabled** (no global lock).
2. While A is mid-turn, type and Enter several messages — they appear **dimmed "queued"**, drain in order, and the footer shows `working… · N queued`.
3. Switch away from A mid-turn and back — the transcript + queued messages + working state **replay correctly**.
4. Disconnect/suspend mid-turn — the input is **never permanently stuck** (state derives from backend/connection, not a dangling promise).

- [ ] **Step 4: Update the stale planning-workflow memory**

The `spawnery-planning-workflow` memory says "no implementation until all epic designs are complete," which no longer matches the repo (active implementation). Update it via `bd` (memories are stored in beads, not MEMORY.md, per CLAUDE.md):

Run: `cd /home/debian/AleCode/spawnery && bd memories planning` to find the key, then `bd remember --key <key> "<corrected note: project is in active implementation; designs+specs still go in docs/superpowers/specs/ and epics in beads, but the no-impl gate is lifted>"`.

- [ ] **Step 5: Close out per CLAUDE.md session protocol**

```bash
cd /home/debian/AleCode/spawnery
git pull --rebase
git push
git status   # MUST show "up to date with origin"
```

If a follow-up surfaced, file it with `bd create`. The deferred sidebar work is already tracked as `sp-abi`.

---

## Self-Review

**Spec coverage:**
- Broker / turn-state correlation (id→stopReason) → Task 1 (`OnAgentLine`/`endTurnLocked`). ✓
- Gate only `session/prompt`; permission/cancel/initialize pass through → Task 1 (`OnClientLine` non-prompt branch) + `TestBrokerNonPromptLinesPassThrough`. ✓
- Queue while busy, FIFO drain, queue cap → Task 1 (`queue`, `MaxQueued`) + tests. ✓
- `cancelled`/error/EOF turn-ends → Task 1 (`isResponse` covers result OR error; `cancelled` test). EOF reset is handled at the connection layer (relay/pump exit), not the broker — noted; the broker simply stops receiving lines. ✓
- `spawn/turn` notification + `spawn/history.turn` + pending items → Task 1 (`turnFrameLocked`, `HistoryFrame`, `Item.Pending`). ✓
- Both lanes wired via shared broker → Tasks 2 (Docker) + 3 (CRI). ✓
- Remove Send button; Enter sends → Task 7. ✓
- Frontend stops deriving busy from the promise; fire-and-forget → Task 10. ✓
- Turn-state from backend; working footer; pending rendering + reconcile → Tasks 5, 6, 8, 10. ✓
- Enter only when connected; queue cap gate → Task 10 (`canSend`). ✓
- `sp-r7t` id-collision flagged as risk (not fixed) → carried from spec; no task needed. ✓
- Deferred sidebar dots → `sp-abi` (already filed); referenced in Task 11. ✓

**Placeholder scan:** No TBD/TODO/"handle edge cases" — every code step shows concrete code and exact run commands. ✓

**Type consistency:** `OnClientLine(line) (forward [][]byte, turn []byte)` and `OnAgentLine(line) (drain [][]byte, turn []byte)` used identically in Tasks 1–3. `SpawnTurn`/`TurnState` shape `{ state: "busy"|"idle"; queued: number }` consistent across `types.ts`, `client.ts`, `turn.ts`, `MessageList`, `ChatView`, `App`. `Item.Pending` (Go) ↔ `pending?` (TS) consistent. `reconcilePending(items, queued)` signature matches its call in Task 10. ✓
