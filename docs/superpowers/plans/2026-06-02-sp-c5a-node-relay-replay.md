# Node-Relay Transcript Replay (sp-c5a) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Record + replay each spawn's transcript at the NODE relay so `spawn/history` survives a browser reload in the Docker/`just dev` lane (the in-pod adapter only covers the CRI lane). Proven by an e2e test.

**Architecture:** Extract slice-2's recorder to `internal/transcript` (shared by the adapter + the node). The node keeps a long-lived per-spawn `transcript.Recorder` registry that outlives client reloads + CP reconnects; in `openSession` it tees relay bytes (chunk→line) into the recorder and replays `spawn/history` to each (re)connecting client before live bytes. Lane-aware: the node records only in the Docker lane (`cfg.InPodAdapter==false`); the CRI lane keeps the adapter (no double-replay).

**Tech Stack:** Go (transcript pkg, node relay, adapter), Playwright e2e.

**Conventions:**
- Commits `git commit --no-verify`. Local-only repo, no push. Do NOT touch `.beads/`.
- Hermetic Go tests; the e2e (Task 3) uses real containers (Docker lane).
- Reference spec: `docs/superpowers/specs/2026-06-02-node-relay-transcript-replay-design.md`.
- The web `Client.onHistory`/`historyToItems` are UNCHANGED — they already consume `spawn/history`.

---

### Task 1: Extract the recorder to `internal/transcript` (shared) + rewire the adapter

**Files:**
- Create: `internal/transcript/recorder.go`
- Create: `internal/transcript/recorder_test.go`
- Modify: `deploy/agent/acpadapter/bridge.go` (use `transcript.*`)
- Modify: `deploy/agent/acpadapter/bridge_test.go` (`item` → `transcript.Item`)
- Delete: `deploy/agent/acpadapter/record.go`, `deploy/agent/acpadapter/record_test.go`

- [ ] **Step 1: Create `internal/transcript/recorder.go`** (the slice-2 recorder, exported verbatim):

```go
// Package transcript records an ACP ndjson conversation (session/prompt + session/update) into a
// coalesced transcript and serializes it as a spawn/history JSON-RPC notification for replay to a
// (re)connecting client. Used by the in-pod acpadapter (CRI lane) and the node relay (Docker lane).
package transcript

import (
	"encoding/json"
	"strings"
	"sync"
)

// MaxItems caps the in-memory transcript. Past the cap the oldest items are dropped and a single
// leading "truncated" marker is kept. On-demand pagination for long histories is a separate epic.
const MaxItems = 500

// Item is one transcript entry. It marshals directly into the spawn/history frame. Roles:
// user | agent | thought | tool | system (system = the truncation marker).
type Item struct {
	Role   string `json:"role"`
	Text   string `json:"text,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// Recorder accumulates a coalesced transcript. All methods are safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	items   []Item
	toolIdx map[string]int // toolCallId -> index in items, for tool_call_update
}

func New() *Recorder { return &Recorder{toolIdx: map[string]int{}} }

// ObserveClientLine records a client->agent ndjson line if it is a session/prompt (one user item).
func (r *Recorder) ObserveClientLine(line []byte) {
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
	r.push(Item{Role: "user", Text: sb.String()})
}

// ObserveAgentLine records an agent->client ndjson line if it is a session/update notification.
func (r *Recorder) ObserveAgentLine(line []byte) {
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

// appendChunk coalesces consecutive same-role chunks into one item. Caller holds r.mu.
func (r *Recorder) appendChunk(role, text string) {
	if text == "" {
		return
	}
	if n := len(r.items); n > 0 && r.items[n-1].Role == role {
		r.items[n-1].Text += text
		return
	}
	r.push(Item{Role: role, Text: text})
}

// push appends an item and enforces the cap. Caller holds r.mu.
func (r *Recorder) push(it Item) {
	r.items = append(r.items, it)
	if len(r.items) <= MaxItems {
		return
	}
	over := len(r.items) - MaxItems
	trimmed := append([]Item{{Role: "system", Text: "earlier history truncated"}}, r.items[over+1:]...)
	r.items = trimmed
	r.toolIdx = map[string]int{}
}

// HistoryFrame returns a newline-terminated spawn/history JSON-RPC notification snapshotting the
// current transcript, or nil if the transcript is empty (nothing to replay).
func (r *Recorder) HistoryFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]Item, len(r.items))
	copy(snap, r.items)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []Item `json:"items"`
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

- [ ] **Step 2: Create `internal/transcript/recorder_test.go`** (the slice-2 record_test, exported API):

```go
package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeFrame(t *testing.T, frame []byte) []Item {
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
			Items []Item `json:"items"`
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
	r := New()
	r.ObserveClientLine(clientPrompt("hello"))
	r.ObserveAgentLine(agentChunk("agent_message_chunk", "He"))
	r.ObserveAgentLine(agentChunk("agent_message_chunk", "llo!"))
	r.ObserveAgentLine(agentChunk("agent_thought_chunk", "hmm"))
	r.ObserveAgentLine([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"search","status":"pending"}}}` + "\n"))
	r.ObserveAgentLine([]byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}` + "\n"))

	items := decodeFrame(t, r.HistoryFrame())
	want := []Item{
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
	r := New()
	if f := r.HistoryFrame(); f != nil {
		t.Fatalf("empty recorder must yield a nil frame, got %q", string(f))
	}
	r.ObserveClientLine([]byte("not json\n"))
	r.ObserveAgentLine([]byte("hello\n"))
	r.ObserveAgentLine([]byte(`{"method":"initialize","id":1}` + "\n"))
	if f := r.HistoryFrame(); f != nil {
		t.Fatalf("recorder must stay empty for non-transcript traffic, got %q", string(f))
	}
}

func TestRecorderCapsAndMarksTruncation(t *testing.T) {
	r := New()
	for i := 0; i < MaxItems+50; i++ {
		r.ObserveClientLine(clientPrompt("p"))
	}
	items := decodeFrame(t, r.HistoryFrame())
	if len(items) != MaxItems {
		t.Fatalf("len=%d want capped at %d", len(items), MaxItems)
	}
	if items[0].Role != "system" || !strings.Contains(items[0].Text, "truncated") {
		t.Fatalf("first item must be the truncation marker, got %+v", items[0])
	}
}
```

- [ ] **Step 3: Run the transcript tests**

Run: `go test ./internal/transcript/ -count=1` → PASS.

- [ ] **Step 4: Delete the adapter's local recorder**

```bash
rm deploy/agent/acpadapter/record.go deploy/agent/acpadapter/record_test.go
```

- [ ] **Step 5: Rewire `deploy/agent/acpadapter/bridge.go` to use `internal/transcript`**

Change the import block from:
```go
import (
	"bufio"
	"io"
	"net"
	"sync"
)
```
to:
```go
import (
	"bufio"
	"io"
	"net"
	"sync"

	"spawnery/internal/transcript"
)
```

Then change the three recorder references:
- In `pump`, the signature `func pump(fromAgent io.Reader, hub *connHub, rec *recorder)` → `func pump(fromAgent io.Reader, hub *connHub, rec *transcript.Recorder)`, and `rec.observeAgent(line)` → `rec.ObserveAgentLine(line)`.
- In `recordingCopy`, `func recordingCopy(toAgent io.Writer, conn io.Reader, rec *recorder)` → `func recordingCopy(toAgent io.Writer, conn io.Reader, rec *transcript.Recorder)`, and `rec.observeClient(line)` → `rec.ObserveClientLine(line)`.
- In `serve`, `rec := newRecorder()` → `rec := transcript.New()`, and `rec.historyFrame()` → `rec.HistoryFrame()`.

(Do NOT change the connHub / line-pump / attach logic — only the recorder type + method names.)

- [ ] **Step 6: Fix `deploy/agent/acpadapter/bridge_test.go`**

`TestServeReplaysHistoryOnReconnect` declares `var m struct { ... Items []item ... }`. Change `[]item` to `[]transcript.Item`, and add `"spawnery/internal/transcript"` to that file's import block.

- [ ] **Step 7: Run the adapter suite + race**

Run:
```bash
go test ./deploy/agent/acpadapter/ -count=1
go test -race ./deploy/agent/acpadapter/ -count=1
```
Expected: PASS (the adapter behaves identically; only the recorder's package/name changed).

- [ ] **Step 8: Whole-repo build + commit**

```bash
go build ./...
git add internal/transcript/ deploy/agent/acpadapter/
git commit --no-verify -m "refactor(transcript): extract recorder to internal/transcript; adapter uses it (sp-c5a)"
```

---

### Task 2: Node-side recorder registry + relay recording/replay (lane-aware)

**Files:**
- Create: `internal/node/record.go`
- Create: `internal/node/record_test.go`
- Modify: `internal/node/attach.go` (Config + Run + runOnce + attacher + openSession + stopSpawn)
- Modify: `cmd/spawnlet/main.go` (set `InPodAdapter`)

- [ ] **Step 1: Write the failing node tests** — create `internal/node/record_test.go`:

```go
package node

import (
	"encoding/json"
	"io"
	"testing"

	"spawnery/internal/spawnlet"
	"spawnery/internal/transcript"
)

func TestLineBufferSplitsAcrossChunks(t *testing.T) {
	var lb lineBuffer
	var got []string
	emit := func(b []byte) { got = append(got, string(b)) }
	lb.feed([]byte("ab"), emit)    // no newline yet
	lb.feed([]byte("c\nde"), emit) // emits "abc\n"
	lb.feed([]byte("f\n"), emit)   // emits "def\n"
	if len(got) != 2 || got[0] != "abc\n" || got[1] != "def\n" {
		t.Fatalf("lines=%q want [abc\\n def\\n]", got)
	}
}

func TestRecordingEndpointTeesAndForwardsByteExact(t *testing.T) {
	rec := transcript.New()
	prompt := []byte(`{"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}}` + "\n")
	update := []byte(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ECHO: hi"}}}}` + "\n")

	recvQ := [][]byte{prompt}
	var sent [][]byte
	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			if len(recvQ) == 0 {
				return nil, io.EOF
			}
			b := recvQ[0]
			recvQ = recvQ[1:]
			return b, nil
		},
		Send: func(b []byte) error { sent = append(sent, append([]byte(nil), b...)); return nil },
	}
	w := recordingEndpoint(ep, rec)

	// client -> agent: the wrapped Recv forwards byte-exact AND records the prompt.
	b, err := w.Recv()
	if err != nil || string(b) != string(prompt) {
		t.Fatalf("recv forward: b=%q err=%v", b, err)
	}
	// agent -> client: the wrapped Send forwards byte-exact AND records the update.
	if err := w.Send(update); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || string(sent[0]) != string(update) {
		t.Fatalf("send forward: %q", sent)
	}

	// the recorder now holds the user prompt + the agent reply.
	var m struct {
		Params struct {
			Items []transcript.Item `json:"items"`
		} `json:"params"`
	}
	frame := rec.HistoryFrame()
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("frame: %v\n%s", err, frame)
	}
	if len(m.Params.Items) != 2 ||
		m.Params.Items[0] != (transcript.Item{Role: "user", Text: "hi"}) ||
		m.Params.Items[1] != (transcript.Item{Role: "agent", Text: "ECHO: hi"}) {
		t.Fatalf("transcript=%+v", m.Params.Items)
	}
}

func TestRecorderRegistryGetOrCreateAndRemove(t *testing.T) {
	reg := newRecorderRegistry()
	a := reg.getOrCreate("sp1")
	if a == nil || reg.getOrCreate("sp1") != a {
		t.Fatal("getOrCreate must return the same recorder per spawn id")
	}
	reg.remove("sp1")
	if reg.getOrCreate("sp1") == a {
		t.Fatal("remove must drop the recorder so a fresh one is created")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/node/ -count=1` → FAIL (`lineBuffer`/`recordingEndpoint`/`newRecorderRegistry` undefined).

- [ ] **Step 3: Create `internal/node/record.go`:**

```go
package node

import (
	"bytes"
	"sync"

	"spawnery/internal/spawnlet"
	"spawnery/internal/transcript"
)

// recorderRegistry holds one long-lived transcript.Recorder per spawn. It outlives client
// reconnects (a browser reload reuses the recorder) and CP reconnects (it is created in node.Run,
// not in the per-connection attacher). Entries are removed only when the spawn is stopped.
type recorderRegistry struct {
	mu  sync.Mutex
	rec map[string]*transcript.Recorder
}

func newRecorderRegistry() *recorderRegistry { return &recorderRegistry{rec: map[string]*transcript.Recorder{}} }

func (r *recorderRegistry) getOrCreate(id string) *transcript.Recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rc := r.rec[id]; rc != nil {
		return rc
	}
	rc := transcript.New()
	r.rec[id] = rc
	return rc
}

func (r *recorderRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.rec, id)
	r.mu.Unlock()
}

// lineBuffer accumulates byte chunks and emits each complete newline-terminated ndjson line. The
// relay forwards opaque chunks; the recorder needs whole lines.
type lineBuffer struct{ buf []byte }

func (l *lineBuffer) feed(p []byte, emit func([]byte)) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return
		}
		line := append([]byte(nil), l.buf[:i+1]...)
		emit(line)
		l.buf = l.buf[i+1:]
	}
}

// recordingEndpoint wraps a StreamEndpoint to TEE its bytes into rec without altering the forwarded
// stream: Recv (client->agent) -> ObserveClientLine; Send (agent->client) -> ObserveAgentLine. Each
// direction has its own lineBuffer touched by a single goroutine (Relay runs Recv and Send in
// separate goroutines), and the recorder is internally mutex-guarded.
func recordingEndpoint(ep spawnlet.StreamEndpoint, rec *transcript.Recorder) spawnlet.StreamEndpoint {
	var clientLB, agentLB lineBuffer
	return spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			b, err := ep.Recv()
			if len(b) > 0 {
				clientLB.feed(b, rec.ObserveClientLine)
			}
			return b, err
		},
		Send: func(b []byte) error {
			if len(b) > 0 {
				agentLB.feed(b, rec.ObserveAgentLine)
			}
			return ep.Send(b)
		},
	}
}
```

- [ ] **Step 4: Run the node tests** — `go test ./internal/node/ -count=1` → PASS (the new tests; there were no prior node tests besides the reconnect one).

- [ ] **Step 5: Wire the registry + replay into `internal/node/attach.go`**

(a) Add the lane flag to `Config`:
```go
type Config struct {
	NodeID     string
	CPURL      string // e.g. http://127.0.0.1:8080
	MaxSpawns  uint32
	AgentImage string
	NodeClass  string
	NodeOwner  string
	InPodAdapter bool // CRI lane: the in-pod adapter records/replays history; the node must NOT (no double-replay). Docker lane = false -> node records.
}
```

(b) Add `recorders *recorderRegistry` to the `attacher` struct (after the `httpc` field):
```go
type attacher struct {
	cfg   Config
	mgr   *spawnlet.Manager
	httpc connect.HTTPClient
	recorders *recorderRegistry // nil in the CRI lane (adapter handles history)

	mu       sync.Mutex
	sessions map[string]*session
	inboxes  map[string]chan []byte
	active   uint32

	sendMu sync.Mutex
	stream *connect.BidiStreamForClient[nodev1.NodeMessage, nodev1.CPMessage]
}
```

(c) Create the registry in `Run` (so it survives reconnects) and thread it to `runOnce`. Change the `Run` body's call site and `runOnce`'s signature:

In `Run`, before the `for` loop:
```go
func Run(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config) error {
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	var recorders *recorderRegistry
	if !cfg.InPodAdapter {
		recorders = newRecorderRegistry() // Docker lane: the node records the transcript
	}
	for {
		start := time.Now()
		err := runOnce(ctx, mgr, httpc, cfg, recorders)
		...
```
(keep the rest of `Run` unchanged.)

Change `runOnce` signature + attacher construction:
```go
func runOnce(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config, recorders *recorderRegistry) error {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	a := &attacher{
		cfg: cfg, mgr: mgr, httpc: httpc, recorders: recorders,
		sessions: map[string]*session{},
		inboxes:  map[string]chan []byte{},
	}
	...
```
(keep the rest of `runOnce` unchanged.)

(d) Replay + record in `openSession`. The current tail of `openSession` is:
```go
	ep := spawnlet.StreamEndpoint{
		Recv: func() ([]byte, error) {
			select {
			case b := <-inbox:
				return b, nil
			case <-rctx.Done():
				return nil, rctx.Err()
			}
		},
		Send: func(b []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{SpawnId: spawnID, Data: append([]byte(nil), b...)}}})
		},
	}
	go func() {
		defer att.Close()
		spawnlet.Relay(rctx, ep, spawnlet.AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	}()
}
```
Replace the `go func()` block (keep the `ep := ...` definition above it) with:
```go
	// In the Docker lane the node records the transcript and replays it to each (re)connecting client
	// (the in-pod adapter does this only in the CRI lane). Replay BEFORE the relay starts so the
	// client gets its history ahead of live bytes (a.send is serialized, so order holds).
	if a.recorders != nil {
		rec := a.recorders.getOrCreate(spawnID)
		if f := rec.HistoryFrame(); f != nil {
			_ = ep.Send(f)
		}
		ep = recordingEndpoint(ep, rec)
	}
	go func() {
		defer att.Close()
		spawnlet.Relay(rctx, ep, spawnlet.AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	}()
}
```

(e) Remove the recorder on stop. In `stopSpawn`, after `_ = a.mgr.Stop(ctx, spawnID)`:
```go
func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	a.closeSession(spawnID)
	_ = a.mgr.Stop(ctx, spawnID)
	if a.recorders != nil {
		a.recorders.remove(spawnID)
	}
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}
```

- [ ] **Step 6: Set `InPodAdapter` in `cmd/spawnlet/main.go`**

In the `node.Config{...}` literal (the CP-attached block), add the field:
```go
		cfg := node.Config{
			NodeID:     env("NODE_ID", "node-1"),
			CPURL:      cpURL,
			MaxSpawns:  4,
			AgentImage: env("AGENT_IMAGE", "spawnery/stubagent:dev"),
			NodeClass:  env("NODE_CLASS", "cloud"),
			NodeOwner:  env("NODE_OWNER", ""),
			InPodAdapter: os.Getenv("CONTAINER_RUNTIME") == "runsc",
		}
```

- [ ] **Step 7: Build + node tests + race + commit**

Run:
```bash
go build ./...
go test ./internal/node/ -count=1
go test -race ./internal/node/ -count=1
```
Expected: build clean; node tests pass (incl. the pre-existing reconnect test); race-clean.

```bash
git add internal/node/ cmd/spawnlet/main.go
git commit --no-verify -m "feat(node): per-spawn transcript recorder + spawn/history replay at the relay (Docker lane) (sp-c5a)"
```

---

### Task 3: e2e — history survives a browser reload (node replay)

**Files:**
- Modify: `web/e2e/spawn-lifecycle.spec.ts` (add the reload-replay test)

The node binary is rebuilt by the e2e `global-setup` (it runs the Docker lane, `InPodAdapter=false`), so the node records. The stub image is unchanged (it speaks ACP ndjson, which the node records). No image rebuild needed for this task, but rebuild stub/sidecar to be safe.

- [ ] **Step 1: Add the reload test** — append to `web/e2e/spawn-lifecycle.spec.ts` (it already has `gotoApp`, `spawnFromMarket`, `rowByName`, and the `clearSpawns` beforeEach):

```typescript
test("conversation history survives a browser reload (node replay)", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  // Reload: the client-side transcript buffer is wiped. The node must replay the transcript.
  await page.reload();
  await expect(page.getByTestId("marketplace")).toBeVisible({ timeout: 20_000 });

  // Re-open the spawn from the sidebar (its name defaults to the app display name "Secret App").
  await rowByName(page, "Secret App").locator('[data-testid^="spawn-select-"]').click();

  // The node replays spawn/history on reconnect -> the prior transcript is restored.
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });
  await expect(page.locator('[data-role="user"]')).toContainText("one");
});
```

- [ ] **Step 2: Run the e2e** (clean stack + DB first):
```bash
cd /home/debian/AleCode/spawnery
make .make/img-stubagent .make/img-sidecar
pkill -f 'bin/cp' 2>/dev/null; pkill -f 'bin/spawnlet' 2>/dev/null
docker rm -f $(docker ps -aq --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
rm -f cp.db; rm -rf .spawns
cd web && npm run test:e2e
```
Expected: ALL e2e specs pass, including the new reload-replay test (now 10 total: 2 chat + 2 marketplace + 6 lifecycle). Report the full breakdown. The new test is the acceptance gate for sp-c5a — it must pass for the right reason (node replay, since the client buffer is wiped by `page.reload()`).

- [ ] **Step 3: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/e2e/spawn-lifecycle.spec.ts
git commit --no-verify -m "test(web-e2e): history survives browser reload via node replay (sp-c5a)"
```

---

## Definition of Done

- Recorder extracted to `internal/transcript`, shared by the adapter (unchanged behavior) + the node.
- The node records each spawn's transcript at the relay and replays `spawn/history` on every client (re)connect, in the Docker lane only (lane-aware via `cfg.InPodAdapter`).
- Conversation history survives a browser reload in `just dev`, proven by the new e2e test (which wipes the client buffer via `page.reload()` and relies on the node replay).
- Go build + `internal/transcript` + `internal/node` (+ adapter) tests green incl. `-race`; full e2e green.

## Out of scope
- Agent memory / lossless suspend / durable cross-process history (sp-3nb).
- History pagination (sp-suc); the 500-item cap is retained.
- Reverting the in-pod adapter's recording (kept for the CRI lane).
