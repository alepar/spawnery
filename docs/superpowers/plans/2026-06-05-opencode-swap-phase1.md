# opencode Swap (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace goose with opencode behind an in-pod adapter that normalizes opencode's HTTP/SSE API to canonical ACP, so the existing web↔CP↔node path drives an opencode session end-to-end with no CP/web changes.

**Architecture:** A new `internal/ocadapter` package implements an ACP *agent server* (the side the node's `internal/acp.Client` talks to) backed by an opencode client (`internal/opencode`). The `deploy/agent/acpadapter` binary becomes thin wiring. Both lanes dial the adapter over TCP; the docker stdio-attach path is removed. The node receives only small agent-neutral cleanups (it already speaks real spec-ACP).

**Tech Stack:** Go 1.26, ACP JSON-RPC 2.0 ndjson (`internal/acp`), opencode `serve` HTTP+SSE (Go SDK `github.com/sst/opencode-sdk-go` or raw REST per `/doc`), Docker, tini, tmux.

**Spec:** `docs/superpowers/specs/2026-06-05-opencode-swap-and-terminal-design.md`
**Beads:** epic `sp-5h3` (children `.1`–`.7`).

---

## File Structure

- Create `internal/opencode/client.go` — typed client for `opencode serve` (health, session list/create, prompt_async, abort, permission respond, SSE event stream). One responsibility: speak opencode's HTTP/SSE.
- Create `internal/opencode/fake_test.go` — an `httptest`-based fake opencode server used by adapter tests (the opencode analogue of `internal/stubagent`).
- Create `internal/ocadapter/server.go` — the ACP agent server: accepts ACP requests/notifications from the node, drives the opencode client, emits ACP `session/update` / `session/request_permission`.
- Create `internal/ocadapter/translate.go` — pure functions mapping opencode events ⇄ ACP messages (unit-tested without I/O).
- Create `internal/acp/server.go` — minimal ACP *server* framing helpers (read requests, write responses/notifications) reusing `internal/acp/codec.go`.
- Modify `deploy/agent/acpadapter/main.go` — wire `ocadapter` instead of the byte bridge; keep TCP listen + reconnect/gap behavior.
- Modify `deploy/agent/acpadapter/bridge.go` — retain `connHub` gap-buffer for the ACP byte stream to the node.
- Modify `deploy/agent/entrypoint.sh` — launch `opencode serve` + adapter under tini; opencode provider config.
- Modify `deploy/agent/Dockerfile` — fetch opencode, add tmux + tini, build adapter.
- Modify `Makefile` — rename `img-goose` target context / add `img-agent`.
- Modify `internal/node/attach.go` / `internal/node/pump.go` / `internal/spawnlet/manager.go` — unify on TCP; remove docker stdio; neutralize goose-named comments; episode-end signal.
- Create `internal/node/acp_conformance_test.go` — canonical-ACP frame fixtures proving the node is agent-neutral.

---

## Task 0: Pin opencode and capture live API shapes (grounding spike)

**Why:** opencode is fast-moving and the Go SDK lags the server (per research §7). We must verify the exact endpoint/event shapes against ONE pinned version before writing client code, so later tasks reference verified field names, not assumptions.

**Files:**
- Create: `docs/superpowers/notes/opencode-api-pinned.md`

- [ ] **Step 1: Pick and pin a version**

```bash
# List recent releases; choose the latest stable. Record the exact tag.
gh release list --repo anomalyco/opencode --limit 10
```
Record the chosen tag (e.g. `vX.Y.Z`) in the notes file as `OPENCODE_VERSION`.

- [ ] **Step 2: Run the server locally and capture `/doc` + health**

```bash
OPENCODE_VERSION=vX.Y.Z   # the pinned tag
# install/download that version's binary (musl static if available), then:
opencode serve --port 4096 --hostname 127.0.0.1 &
sleep 2
curl -s http://127.0.0.1:4096/global/health
curl -s http://127.0.0.1:4096/doc > /tmp/opencode-openapi.json
```
Paste the health JSON and the relevant `/doc` paths (`/session`, `/session/{id}/prompt_async`, `/session/{id}/message`, `/session/{id}/abort`, `/session/{id}/permissions/{permissionID}`, `/event`) into the notes file.

- [ ] **Step 3: Capture real SSE event shapes for the ops we map**

```bash
# Subscribe, then in another shell create a session and send a prompt; record events.
curl -sN http://127.0.0.1:4096/event > /tmp/events.ndjson &
SID=$(curl -s -X POST http://127.0.0.1:4096/session -d '{"title":"probe"}' -H 'content-type: application/json' | jq -r '.id')
curl -s -X POST "http://127.0.0.1:4096/session/$SID/prompt_async" \
  -H 'content-type: application/json' \
  -d '{"parts":[{"type":"text","text":"say hi then run `ls` (to trigger a permission)"}]}'
sleep 8; kill %2 2>/dev/null
```
Record, in the notes file, the exact JSON for: `server.connected`, `message.part.updated` (note the `delta` and `part.type` discriminators for text/reasoning/tool), `permission.asked`/`permission.updated` (note `permissionID`, `sessionID`, tool, and the option/kind shape), `session.status`, `session.idle`. These shapes are the contract the translate functions in Task 4–6 target.

- [ ] **Step 4: Decide SDK vs REST and pin it**

Decide per endpoint: use `github.com/sst/opencode-sdk-go` where it matches the pinned server; fall back to raw REST (net/http) for `prompt_async` and any event variant the SDK union lacks. Record the decision + SDK version (if used) in the notes file.

- [ ] **Step 5: Commit the notes**

```bash
git add docs/superpowers/notes/opencode-api-pinned.md
git commit -m "docs(opencode): pin version and capture live API/SSE shapes (sp-5h3)"
```

---

## Task 1: ACP server framing helpers

**Files:**
- Create: `internal/acp/server.go`
- Test: `internal/acp/server_test.go`

- [ ] **Step 1: Write the failing test**

```go
package acp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestServerReadsRequestAndWritesResponse(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var out bytes.Buffer
	s := NewServer(in, &out)

	msg, err := s.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Method != "initialize" || msg.ID == nil || *msg.ID != 1 {
		t.Fatalf("unexpected msg: %+v", msg)
	}
	if err := s.Respond(*msg.ID, map[string]any{"protocolVersion": 1}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	line, _ := bufio.NewReader(&out).ReadString('\n')
	if !strings.Contains(line, `"id":1`) || !strings.Contains(line, `"protocolVersion":1`) {
		t.Fatalf("bad response line: %s", line)
	}
}

func TestServerWritesNotification(t *testing.T) {
	var out bytes.Buffer
	s := NewServer(strings.NewReader(""), &out)
	if err := s.Notify("session/update", map[string]any{"sessionId": "s1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"method":"session/update"`) || strings.Contains(got, `"id"`) {
		t.Fatalf("notification should have method and no id: %s", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/acp/ -run TestServer -v`
Expected: FAIL — `NewServer` undefined.

- [ ] **Step 3: Implement `internal/acp/server.go`**

```go
package acp

import (
	"encoding/json"
	"io"
)

// Server is the agent side of ACP: it reads client requests/notifications and
// writes responses and notifications, reusing the shared codec. It is the
// counterpart to Client and is used by the opencode adapter.
type Server struct {
	r *Reader
	w io.Writer
}

func NewServer(r io.Reader, w io.Writer) *Server { return &Server{r: NewReader(r), w: w} }

// Read returns the next message from the client (request or notification).
func (s *Server) Read() (Message, error) { return s.r.ReadMessage() }

// Respond writes a successful JSON-RPC response for the given request id.
func (s *Server) Respond(id int, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return WriteMessage(s.w, Message{ID: &id, Result: b})
}

// RespondError writes a JSON-RPC error response.
func (s *Server) RespondError(id, code int, message string) error {
	return WriteMessage(s.w, Message{ID: &id, Error: &Error{Code: code, Message: message}})
}

// Notify writes a notification (no id).
func (s *Server) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return WriteMessage(s.w, Message{Method: method, Params: b})
}

// Request writes a server-initiated request (used for session/request_permission).
func (s *Server) Request(id int, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return WriteMessage(s.w, Message{ID: &id, Method: method, Params: b})
}
```

If `Message` lacks `Error`/`Result`/`Params` fields or an `Error` type, add them in `internal/acp/codec.go` to match (verify against the existing struct first; the `Client` already reads `m.Error`/`m.Result`, so they exist).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/acp/ -run TestServer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/acp/server.go internal/acp/server_test.go internal/acp/codec.go
git commit -m "feat(acp): add agent-side Server framing helpers (sp-5h3)"
```

---

## Task 2: Pure translation functions (opencode → ACP)

**Files:**
- Create: `internal/ocadapter/translate.go`
- Test: `internal/ocadapter/translate_test.go`

Uses the verified shapes from Task 0. The opencode event JSON below matches the documented shapes; **adjust field names to the Task-0 capture if they differ.**

- [ ] **Step 1: Write the failing test**

```go
package ocadapter

import "testing"

func TestTextDeltaToSessionUpdate(t *testing.T) {
	// message.part.updated with a text delta -> ACP agent_message_chunk
	params, ok := OpencodeEventToACP(Event{
		Type: "message.part.updated",
		Part: Part{Type: "text", Text: "hello"},
		SessionID: "s1",
	})
	if !ok {
		t.Fatal("expected a translated update")
	}
	if params.SessionID != "s1" || params.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("bad mapping: %+v", params)
	}
	if params.Update.Content.Text != "hello" {
		t.Fatalf("bad text: %q", params.Update.Content.Text)
	}
}

func TestReasoningDeltaMapsToThought(t *testing.T) {
	params, ok := OpencodeEventToACP(Event{Type: "message.part.updated", Part: Part{Type: "reasoning", Text: "thinking"}, SessionID: "s1"})
	if !ok || params.Update.SessionUpdate != "agent_thought_chunk" {
		t.Fatalf("reasoning should map to agent_thought_chunk: %+v", params)
	}
}

func TestPermissionAskedToOptions(t *testing.T) {
	opts := PermissionToACPOptions() // canonical 4 ACP option kinds
	kinds := map[string]bool{}
	for _, o := range opts {
		kinds[o.Kind] = true
	}
	for _, want := range []string{"allow_once", "allow_always", "reject_once", "reject_always"} {
		if !kinds[want] {
			t.Fatalf("missing ACP option kind %q in %+v", want, opts)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ocadapter/ -run TestText -v`
Expected: FAIL — undefined types/functions.

- [ ] **Step 3: Implement `internal/ocadapter/translate.go`**

```go
package ocadapter

// Event is the subset of an opencode /event SSE payload the adapter consumes.
// Field names verified against docs/superpowers/notes/opencode-api-pinned.md.
type Event struct {
	Type      string
	SessionID string
	Part      Part
	// permission fields (for permission.asked/updated)
	PermissionID string
	Tool         string
}

type Part struct {
	Type string // "text" | "reasoning" | "tool" | ...
	Text string
}

// ACPUpdateParams is the params of an ACP session/update notification.
type ACPUpdateParams struct {
	SessionID string     `json:"sessionId"`
	Update    ACPUpdate  `json:"update"`
}

type ACPUpdate struct {
	SessionUpdate string     `json:"sessionUpdate"`
	Content       ACPContent `json:"content,omitempty"`
}

type ACPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ACPOption is one permission option offered to the client (canonical ACP kinds).
type ACPOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// OpencodeEventToACP maps a content delta to an ACP session/update. Returns
// (params, true) for events that produce an update; (_, false) to drop.
func OpencodeEventToACP(e Event) (ACPUpdateParams, bool) {
	if e.Type != "message.part.updated" {
		return ACPUpdateParams{}, false
	}
	var kind string
	switch e.Part.Type {
	case "text":
		kind = "agent_message_chunk"
	case "reasoning":
		kind = "agent_thought_chunk"
	default:
		return ACPUpdateParams{}, false // tool/file parts handled separately later
	}
	return ACPUpdateParams{
		SessionID: e.SessionID,
		Update: ACPUpdate{
			SessionUpdate: kind,
			Content:       ACPContent{Type: "text", Text: e.Part.Text},
		},
	}, true
}

// PermissionToACPOptions returns the canonical four ACP permission options the
// node's pickPermOption selects from (it matches on Kind containing allow/reject).
func PermissionToACPOptions() []ACPOption {
	return []ACPOption{
		{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
		{OptionID: "reject_once", Name: "Reject once", Kind: "reject_once"},
		{OptionID: "reject_always", Name: "Reject always", Kind: "reject_always"},
	}
}

// ACPOptionIDToOpencodeResponse maps the optionId the node selected back to the
// opencode permission response value ("once"|"always"|"reject").
func ACPOptionIDToOpencodeResponse(optionID string) string {
	switch optionID {
	case "allow_once":
		return "once"
	case "allow_always":
		return "always"
	default:
		return "reject"
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ocadapter/ -run 'TestText|TestReasoning|TestPermission' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ocadapter/translate.go internal/ocadapter/translate_test.go
git commit -m "feat(ocadapter): pure opencode->ACP translation + permission kind mapping (sp-5h3)"
```

---

## Task 3: opencode client + fake server

**Files:**
- Create: `internal/opencode/client.go`
- Create: `internal/opencode/fake_test.go`
- Test: `internal/opencode/client_test.go`

- [ ] **Step 1: Write the fake server + failing test**

```go
// fake_test.go
package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

// newFake returns an httptest server emulating the pinned opencode endpoints we use.
func newFake() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "fake"})
	})
	sessions := []map[string]any{}
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s := map[string]any{"id": "sess-1", "title": "t"}
			sessions = append(sessions, s)
			_ = json.NewEncoder(w).Encode(s)
			return
		}
		_ = json.NewEncoder(w).Encode(sessions) // GET list
	})
	return httptest.NewServer(mux)
}
```

```go
// client_test.go
package opencode

import "testing"

func TestHealth(t *testing.T) {
	srv := newFake(); defer srv.Close()
	c := New(srv.URL)
	if err := c.Health(); err != nil {
		t.Fatalf("health: %v", err)
	}
}

func TestDiscoverOrCreateCreatesWhenEmpty(t *testing.T) {
	srv := newFake(); defer srv.Close()
	c := New(srv.URL)
	id, err := c.DiscoverOrCreateSession("title")
	if err != nil || id != "sess-1" {
		t.Fatalf("got id=%q err=%v", id, err)
	}
}

func TestDiscoverOrCreateReusesExisting(t *testing.T) {
	srv := newFake(); defer srv.Close()
	c := New(srv.URL)
	if _, err := c.DiscoverOrCreateSession("t"); err != nil { // create one
		t.Fatal(err)
	}
	id, err := c.DiscoverOrCreateSession("t") // should reuse, not create a new id
	if err != nil || id != "sess-1" {
		t.Fatalf("reuse failed: id=%q err=%v", id, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/opencode/ -v`
Expected: FAIL — `New` undefined.

- [ ] **Step 3: Implement `internal/opencode/client.go`**

```go
package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	base string
	hc   *http.Client
}

func New(baseURL string) *Client {
	return &Client{base: baseURL, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Health() error {
	resp, err := c.hc.Get(c.base + "/global/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("opencode health: %s", resp.Status)
	}
	return nil
}

// DiscoverOrCreateSession reuses the first existing session if any, else creates one.
func (c *Client) DiscoverOrCreateSession(title string) (string, error) {
	resp, err := c.hc.Get(c.base + "/session")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var existing []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&existing); err == nil && len(existing) > 0 {
		return existing[0].ID, nil
	}
	body, _ := json.Marshal(map[string]any{"title": title})
	cr, err := c.hc.Post(c.base+"/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer cr.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(cr.Body).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// PromptAsync sends a text prompt without waiting (results arrive via SSE).
func (c *Client) PromptAsync(sessionID, text string) error {
	body, _ := json.Marshal(map[string]any{
		"parts": []map[string]any{{"type": "text", "text": text}},
	})
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/prompt_async", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("prompt_async: %s", resp.Status)
	}
	return nil
}

// Abort cancels the in-flight turn.
func (c *Client) Abort(sessionID string) error {
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/abort", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RespondPermission answers a permission request ("once"|"always"|"reject").
func (c *Client) RespondPermission(sessionID, permissionID, response string) error {
	body, _ := json.Marshal(map[string]any{"response": response})
	resp, err := c.hc.Post(c.base+"/session/"+sessionID+"/permissions/"+permissionID, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("respond permission: %s", resp.Status)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/opencode/ -v`
Expected: PASS (Health, DiscoverOrCreate create + reuse).

- [ ] **Step 5: Commit**

```bash
git add internal/opencode/
git commit -m "feat(opencode): HTTP client (health, discover-or-create, prompt_async, abort, perms) + fake (sp-5h3)"
```

---

## Task 4: SSE event stream consumer

**Files:**
- Modify: `internal/opencode/client.go`
- Modify: `internal/opencode/fake_test.go` (add `/event` SSE)
- Test: `internal/opencode/events_test.go`

- [ ] **Step 1: Add SSE to the fake + failing test**

Add to `fake_test.go` `newFake()` mux:

```go
mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
	f, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	// server.connected, then one text delta, then idle.
	for _, line := range []string{
		`{"type":"server.connected"}`,
		`{"type":"message.part.updated","sessionID":"sess-1","part":{"type":"text","text":"hi"}}`,
		`{"type":"session.idle","sessionID":"sess-1"}`,
	} {
		_, _ = w.Write([]byte("data: " + line + "\n\n"))
		if f != nil {
			f.Flush()
		}
	}
})
```

```go
// events_test.go
package opencode

import "testing"

func TestEventsStreamsUntilIdle(t *testing.T) {
	srv := newFake(); defer srv.Close()
	c := New(srv.URL)
	var got []RawEvent
	err := c.Events(testCtx(t), func(e RawEvent) { got = append(got, e) })
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) < 2 || got[1].Type != "message.part.updated" {
		t.Fatalf("missing delta: %+v", got)
	}
}
```

(Provide `testCtx` helper returning a context cancelled on `t.Cleanup`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/opencode/ -run TestEvents -v`
Expected: FAIL — `Events`/`RawEvent` undefined.

- [ ] **Step 3: Implement SSE consumer**

```go
// in client.go
import (
	"bufio"
	"context"
	"strings"
)

// RawEvent is one decoded SSE data line from /event. Type discriminates; Data is the raw JSON.
type RawEvent struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// Events subscribes to /event and calls fn for each event until the stream ends or ctx is done.
// Caller is responsible for reconnect/backoff (the Go SDK does not auto-reconnect).
func (c *Client) Events(ctx context.Context, fn func(RawEvent)) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/event", nil)
	resp, err := (&http.Client{}).Do(req) // no client timeout for a long-lived stream
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := json.RawMessage(strings.TrimPrefix(line, "data: "))
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &head)
		fn(RawEvent{Type: head.Type, Raw: raw})
	}
	return sc.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/opencode/ -run TestEvents -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/opencode/
git commit -m "feat(opencode): SSE /event consumer (sp-5h3)"
```

---

## Task 5: Adapter server — prompt + streaming + idle (end-to-end against the fake)

**Files:**
- Create: `internal/ocadapter/server.go`
- Test: `internal/ocadapter/server_test.go`

- [ ] **Step 1: Write the failing integration test (uses opencode fake + ACP pipe)**

```go
package ocadapter

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"

	"spawnery/internal/opencode"
)

func TestAdapterServesACPPromptAndStreams(t *testing.T) {
	oc := opencode.NewFakeForAdapter(t) // exported fake helper; see Step 3
	defer oc.Close()

	nodeR, adapterW := io.Pipe() // adapter -> node
	adapterR, nodeW := io.Pipe() // node -> adapter

	a := New(opencode.New(oc.URL))
	go a.Serve(adapterR, adapterW) // ACP server loop

	// node: initialize, session/new, session/prompt
	_, _ = io.WriteString(nodeW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	_, _ = io.WriteString(nodeW, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`+"\n")
	_, _ = io.WriteString(nodeW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"sess-1","prompt":[{"type":"text","text":"hi"}]}}`+"\n")

	br := bufio.NewReader(nodeR)
	deadline := time.Now().Add(3 * time.Second)
	sawDelta := false
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "agent_message_chunk") && strings.Contains(line, "hi") {
			sawDelta = true
			break
		}
	}
	if !sawDelta {
		t.Fatal("never saw streamed agent_message_chunk for the prompt")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ocadapter/ -run TestAdapterServes -v`
Expected: FAIL — `New`/`Serve`/`NewFakeForAdapter` undefined.

- [ ] **Step 3: Implement `server.go` + export the fake**

Move `newFake` into a reusable `opencode.NewFakeForAdapter(t)` (exported test helper in a `internal/opencode/fake.go` guarded by a build tag or plain file) that also serves `/event` with a text delta keyed off the last prompt. Then:

```go
package ocadapter

import (
	"encoding/json"
	"io"

	"spawnery/internal/acp"
	"spawnery/internal/opencode"
)

type Adapter struct {
	oc        *opencode.Client
	sessionID string
}

func New(oc *opencode.Client) *Adapter { return &Adapter{oc: oc} }

// Serve runs the ACP agent loop: read client messages, drive opencode, stream updates back.
func (a *Adapter) Serve(r io.Reader, w io.Writer) error {
	srv := acp.NewServer(r, w)
	// Start the SSE pump -> ACP session/update notifications.
	go a.pump(srv)
	for {
		m, err := srv.Read()
		if err != nil {
			return err
		}
		switch m.Method {
		case "initialize":
			if err := a.oc.Health(); err != nil {
				_ = srv.RespondError(*m.ID, -32000, "opencode not ready")
				continue
			}
			id, err := a.oc.DiscoverOrCreateSession("spawnery")
			if err != nil {
				_ = srv.RespondError(*m.ID, -32000, err.Error())
				continue
			}
			a.sessionID = id
			_ = srv.Respond(*m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
		case "session/new":
			_ = srv.Respond(*m.ID, map[string]any{"sessionId": a.sessionID})
		case "session/prompt":
			text := extractPromptText(m.Params)
			if err := a.oc.PromptAsync(a.sessionID, text); err != nil {
				_ = srv.RespondError(*m.ID, -32000, err.Error())
				continue
			}
			// turn-end response is sent when session.idle arrives (see pump); for the
			// minimal slice, respond immediately and rely on streamed updates.
			_ = srv.Respond(*m.ID, map[string]any{"stopReason": "end_turn"})
		}
	}
}

func (a *Adapter) pump(srv *acp.Server) {
	// reconnect loop omitted for brevity in the slice; Task 8 adds backoff.
	_ = a.oc.Events(contextTODO(), func(e opencode.RawEvent) {
		var ev Event
		decodeRawEvent(e, &ev)
		if p, ok := OpencodeEventToACP(ev); ok {
			_ = srv.Notify("session/update", p)
		}
	})
}
```

Provide `extractPromptText`, `decodeRawEvent`, and a `contextTODO()` (replaced by a real cancellable context in Task 8). Keep these small and tested where logic exists.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ocadapter/ -run TestAdapterServes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ocadapter/ internal/opencode/
git commit -m "feat(ocadapter): ACP server drives opencode prompt + streams deltas (sp-5h3)"
```

---

## Task 6: Permission round-trip

**Files:**
- Modify: `internal/ocadapter/server.go`
- Modify: `internal/opencode/fake.go` (emit `permission.asked`, accept the POST)
- Test: `internal/ocadapter/perm_test.go`

- [ ] **Step 1: Failing test** — fake emits `permission.asked`; assert the adapter sends an ACP `session/request_permission` with the four canonical option kinds; reply with `{"outcome":{"outcome":"selected","optionId":"allow_once"}}`; assert the fake received `POST /permissions/<id>` with `{"response":"once"}`.

```go
func TestPermissionRoundTrip(t *testing.T) { /* drive as in Task 5; assert option kinds out, POST response="once" recorded by fake */ }
```

- [ ] **Step 2: Run** `go test ./internal/ocadapter/ -run TestPermission -v` → FAIL.

- [ ] **Step 3: Implement** — in `pump`, when `e.Type == "permission.asked"` send `srv.Request(nextID, "session/request_permission", {sessionId, toolCall:{title:e.Tool}, options: PermissionToACPOptions()})`; track `nextID -> permissionID`. In `Serve`, handle the client's response message (matching that id): read `result.outcome.optionId`, call `a.oc.RespondPermission(sessionID, permissionID, ACPOptionIDToOpencodeResponse(optionID))`. On connect, also `GET` pending permissions and emit them (bug #21154 workaround).

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `feat(ocadapter): permission request/response round-trip with ACP kinds (sp-5h3)`.

---

## Task 7: Wire the adapter binary; remove the byte bridge

**Files:**
- Modify: `deploy/agent/acpadapter/main.go`
- Modify: `deploy/agent/acpadapter/bridge.go` (keep `connHub` for the node TCP conn gap buffer)
- Modify: `deploy/agent/acpadapter/main_test.go`

- [ ] **Step 1: Update the binary test** — `main_test.go` should start the adapter with `ACP_LISTEN=tcp://...` and `OPENCODE_BASE_URL=<fake url>` (run the `opencode.NewFakeForAdapter` in-process via a tiny helper binary, or point at a stub), connect, send an ACP `initialize`, and expect a JSON-RPC response with `protocolVersion`. Replace the old `cat`-echo expectation.

- [ ] **Step 2: Run** `go test ./deploy/agent/acpadapter/ -v` → FAIL.

- [ ] **Step 3: Implement** — `main.go`: parse `ACP_LISTEN` (TCP only now), read `OPENCODE_BASE_URL` (default `http://127.0.0.1:4096`), build `opencode.New(...)`, and for each accepted node connection call `ocadapter.New(oc).Serve(conn, conn)`. Keep one-conn-at-a-time + `connHub` gap-buffer semantics for reconnect. Delete the goose-stdio `exec.Command(os.Args[1], ...)` agent-launch path.

- [ ] **Step 4: Run** `go test ./deploy/agent/acpadapter/ -v` → PASS.

- [ ] **Step 5: Commit** `refactor(acpadapter): drive opencode via ocadapter; drop stdio byte-bridge (sp-5h3)`.

---

## Task 8: Reconnect/backoff + cancel + busy synthesis

**Files:**
- Modify: `internal/ocadapter/server.go`
- Test: `internal/ocadapter/server_test.go`

- [ ] **Step 1: Failing tests** — (a) SSE drop then re-subscribe (fake closes `/event` once, adapter reconnects and still delivers a later delta); (b) ACP `session/cancel` → fake records `POST /abort`; (c) a fake-emitted `session.status=busy` not initiated by the node produces an ACP turn-busy `session/update` (busy synthesis for TUI-originated turns).

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** — wrap `pump` in a backoff loop (1s→30s, reset on healthy run) with a real `context.Context`; on reconnect backfill via `GET /session/:id/message`. Handle `cancel`/`session/cancel` → `a.oc.Abort`. Map `session.status`/`session.idle` to ACP turn state notifications.

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `feat(ocadapter): SSE reconnect+backfill, cancel, busy synthesis (sp-5h3)`.

---

## Task 9: Unify on TCP; remove docker stdio path

**Files:**
- Modify: `internal/node/attach.go`, `internal/spawnlet/manager.go` (the docker-API stdio attach), `internal/node/pump.go` wiring
- Test: existing `internal/node/*_test.go`, `internal/spawnlet/manager_test.go`

- [ ] **Step 1: Find the docker stdio attach** — `grep -rn "ContainerAttach\|stdcopy\|docker.*attach\|StdinPipe" internal/spawnlet internal/node`. Write/adjust a test asserting the docker-lane manager dials the agent over **TCP** (container IP:port) like the runsc lane, not via Docker-API stdio.

- [ ] **Step 2: Run** the targeted test → FAIL.

- [ ] **Step 3: Implement** — make the docker lane set the agent address to the container's reachable TCP endpoint and dial it; delete the Docker-API attach code and the abstract-UDS default. Both lanes now share one transport path.

- [ ] **Step 4: Run** `go test ./internal/node/... ./internal/spawnlet/... -count=1` → PASS.

- [ ] **Step 5: Commit** `refactor(node): unify both lanes on TCP ACP; remove docker stdio attach (sp-5h3)`.

---

## Task 10: Node neutralization + episode-end signal + conformance test

**Files:**
- Modify: `internal/node/pump.go` (comments/field docs only)
- Modify: `internal/node/attach.go` (episode-end signal)
- Create: `internal/node/acp_conformance_test.go`

- [ ] **Step 1: Conformance test (failing first if behavior needs change)** — feed the pump canonical-ACP frames (a permission request with the four standard kinds; an `agent_message_chunk`; an `agent_thought_chunk`) and assert it produces the correct spawnery frames and selects the right optionId via `pickPermOption`. No `goose`-specific assumptions.

```go
func TestPumpIsAgentNeutralACP(t *testing.T) { /* drive pump with canonical ACP, assert frames + optionId selection */ }
```

- [ ] **Step 2: Run** → PASS or FAIL (if FAIL, the node had a real coupling — fix it).

- [ ] **Step 3: Implement** — rename `goose`-named comments/field docs to agent-neutral. Episode-end: ensure the lifecycle FSM does **not** treat the long-lived adapter/opencode connection staying up as "agent exited"; spawn-done is driven by CP stop/suspend + idle reaper. Adjust `attach.go`/`pump.go` `exitFn` semantics accordingly (the agent connection dropping is a reconnectable transport event, not episode end).

- [ ] **Step 4: Run** `go test ./internal/node/... -count=1` → PASS.

- [ ] **Step 5: Commit** `refactor(node): agent-neutral ACP naming + episode-end signal + conformance test (sp-5h3)`.

---

## Task 11: Agent image — opencode + tmux + tini supervisor + provider config

**Files:**
- Modify: `deploy/agent/Dockerfile`
- Modify: `deploy/agent/entrypoint.sh`
- Modify: `Makefile`

- [ ] **Step 1: Dockerfile** — replace the goose fetch stage with an opencode fetch stage pinned to `OPENCODE_VERSION` from Task 0; `apt-get install -y --no-install-recommends tmux tini ca-certificates`; keep the gobuild stage building `acpadapter`; copy `opencode`, `acpadapter`, `entrypoint.sh`. `ENTRYPOINT ["/usr/bin/tini","--","/entrypoint.sh"]`.

- [ ] **Step 2: entrypoint.sh** — configure opencode's provider to use the sidecar OpenAI-compatible endpoint (`OPENAI_BASE_URL=http://127.0.0.1:8080/v1` or opencode's provider config equivalent — set per Task-0 notes) + injected key; then:

```sh
opencode serve --port 4096 --hostname 127.0.0.1 &
exec /usr/local/bin/acpadapter   # reads ACP_LISTEN + OPENCODE_BASE_URL=http://127.0.0.1:4096
```

(tini reaps; if opencode dies, the adapter's opencode calls fail → ACP errors → node sees agent-down.)

- [ ] **Step 3: Makefile** — point the agent image stamp at the updated Dockerfile (rename `img-goose` → `img-agent`, tag `spawnery/agent:dev`); update `images:` list. Update any references in e2e/test harness to the new tag.

- [ ] **Step 4: Build** — `make .make/img-agent` (or `make images`). Expected: image builds; `docker run --rm spawnery/agent:dev which opencode tmux tini` lists all three.

- [ ] **Step 5: Commit** `build(agent): opencode+tmux+tini image; entrypoint runs serve+adapter (sp-5h3)`.

---

## Task 12: Suspend/resume — opencode state dir capture + quiesce

**Files:**
- Modify: `deploy/agent/entrypoint.sh` (set opencode data dir into a captured mount)
- Modify: the suspend path in `internal/spawnlet/manager.go` (quiesce before dirty-tree capture)
- Test: `internal/spawnlet/manager_test.go` (or the suspend test)

- [ ] **Step 1: Locate opencode's data dir** (Task-0 notes) and set it via env/config to live under a dirty-tree-captured mount (e.g. `/app/.opencode` or the data mount root). Add a test asserting a created session's SQLite file path is within a captured mount.

- [ ] **Step 2: Run** → FAIL if dir is outside captured mounts.

- [ ] **Step 3: Implement** — (a) set the data dir env in `entrypoint.sh`; (b) on suspend, before snapshot, quiesce opencode: send a checkpoint (SQLite `PRAGMA wal_checkpoint(TRUNCATE)` via opencode if exposed, or stop the server) so no torn WAL is captured. Verify resume → adapter `DiscoverOrCreateSession` reuses the restored session.

- [ ] **Step 4: Run** the suspend/resume test → PASS.

- [ ] **Step 5: Commit** `fix(suspend): capture opencode SQLite in a persisted mount + quiesce before snapshot (sp-5h3)`.

---

## Task 13: opencode e2e (both lanes) + goose witness

**Files:**
- Create: `internal/spawnlet/e2e_opencode_test.go` (build tag `e2e`)
- Keep: the existing `*goose*` e2e as the neutrality witness (skip if goose image absent)

- [ ] **Step 1: Write the e2e** — with `make images` building `spawnery/agent:dev`, bring up sidecar + agent, start a node, open a session via CP/node, send a prompt from the web/CP frame layer, and assert streamed `agent`/`turn` frames come back. Add a bidirectional check stub (full TUI visibility is asserted in Phase 2; here assert the adapter forwards a server-side event the node didn't initiate).

- [ ] **Step 2: Run** `just test-e2e` → the opencode e2e FAILs first (no assertions satisfied) then PASSES after wiring.

- [ ] **Step 3: Implement** any glue revealed by the e2e (env wiring, addresses).

- [ ] **Step 4: Run** `just test-e2e` and `just lint-go` → PASS / clean.

- [ ] **Step 5: Commit** `test(e2e): opencode web-drive on both lanes; keep goose witness (sp-5h3)`.

---

## Phase 1 Done-When

- `go test ./... -count=1` and `just test-e2e` pass; `just lint-go` clean.
- Web UI (via CP/node, unchanged) drives an opencode conversation: streaming, permissions, busy/idle, cancel.
- Both runc and runsc lanes dial the adapter over TCP; no docker stdio path remains.
- `sp-5h3.1`–`.7` closed.

## Self-Review notes (gaps to watch during execution)
- Task 0 outputs are load-bearing for Tasks 2–6/11–12 (exact opencode field names, data-dir path, provider-config mechanism). Do Task 0 first; adjust the concrete JSON in later tasks to the captured shapes.
- Tool-call parts (`part.type == "tool"`) are only partially mapped (Task 2 drops them); if the web UI needs tool frames, extend `OpencodeEventToACP` + add a test before Task 13.
- `prompt_async` turn-end: the slice responds to `session/prompt` immediately and streams via SSE; if the node's turn FSM needs the response to coincide with `session.idle`, move the `session/prompt` response into the pump on idle (Task 8) and add a test.
