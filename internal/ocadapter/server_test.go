package ocadapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/opencode"
)

// harness wires a node-side ACP pipe to an adapter backed by the fake. A single
// drainer goroutine continuously reads every adapter->node line into a channel
// so the pipe never blocks the adapter (which would deadlock).
type harness struct {
	in    io.Writer
	lines chan string
	fake  *opencode.Fake
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := opencode.NewFake("/app")
	t.Cleanup(fake.Close)

	adapterR, nodeW := io.Pipe() // node -> adapter
	nodeR, adapterW := io.Pipe() // adapter -> node
	a := New(opencode.New(fake.URL), "/app", "")
	go func() { _ = a.Serve(adapterR, adapterW) }()

	// Cleanups run LIFO: this (registered after fake.Close) runs FIRST, closing
	// the pipes so Serve returns and cancels its SSE pump — otherwise the open
	// /event connection makes fake.Close() (httptest) block forever.
	t.Cleanup(func() {
		_ = adapterR.Close()
		_ = nodeW.Close()
		_ = adapterW.Close()
		_ = nodeR.Close()
		time.Sleep(50 * time.Millisecond) // let the SSE handler unwind
	})

	lines := make(chan string, 256)
	go func() {
		br := bufio.NewReader(nodeR)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	return &harness{in: nodeW, lines: lines, fake: fake}
}

func (h *harness) send(s string) { _, _ = io.WriteString(h.in, s+"\n") }

// await scans incoming lines until pred matches or it times out.
func (h *harness) await(t *testing.T, pred func(acp.Message, string) bool) acp.Message {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatal("adapter closed the connection before the expected message")
			}
			var m acp.Message
			_ = json.Unmarshal([]byte(line), &m)
			if pred(m, line) {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for expected ACP message")
		}
	}
}

func idIs(n int) func(acp.Message, string) bool {
	return func(m acp.Message, _ string) bool { return m.ID != nil && *m.ID == n }
}

func TestAdapterInitializeAndSession(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	m := h.await(t, idIs(1))
	if m.Result == nil || !strings.Contains(string(m.Result), "protocolVersion") {
		t.Fatalf("bad initialize result: %s", string(m.Result))
	}
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	m = h.await(t, idIs(2))
	if !strings.Contains(string(m.Result), "ses_fake") {
		t.Fatalf("session/new should return the opencode session id: %s", string(m.Result))
	}
}

func TestAdapterPromptStreamsDeltaThenTurnEnd(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"hi"}]}}`)

	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "agent_message_chunk") && strings.Contains(line, `"hi"`)
	})
	m := h.await(t, idIs(3))
	if !strings.Contains(string(m.Result), "end_turn") {
		t.Fatalf("expected end_turn response, got %s", string(m.Result))
	}
}

func TestAdapterPermissionRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))

	time.Sleep(150 * time.Millisecond) // let the pump subscribe to /event
	h.fake.EmitPermissionAsked("ses_fake1", "per_1")

	req := h.await(t, func(m acp.Message, _ string) bool { return m.Method == "session/request_permission" })
	if req.ID == nil {
		t.Fatal("permission request must have an id")
	}
	line := mustLine(req)
	for _, k := range []string{"allow_once", "allow_always", "reject_once", "reject_always"} {
		if !strings.Contains(line, k) {
			t.Fatalf("missing option kind %q in %s", k, line)
		}
	}
	h.send(`{"jsonrpc":"2.0","id":` + itoa(*req.ID) + `,"result":{"outcome":{"outcome":"selected","optionId":"allow_once"}}}`)

	deadline := time.After(2 * time.Second)
	for {
		if pr := h.fake.PermResponses(); len(pr) == 1 {
			if pr[0].Response != "once" || pr[0].PermissionID != "per_1" {
				t.Fatalf("bad recorded permission: %+v", pr[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("opencode never received the permission response")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestAdapterEchoesTUIUserMessage(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	time.Sleep(150 * time.Millisecond) // let the pump subscribe

	// A user message typed in the TUI (not submitted by us) must be echoed to the web.
	h.fake.EmitUserMessage("ses_fake1", "msg_t", "prt_t", "hello from the TUI")
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "user_message_chunk") && strings.Contains(line, "hello from the TUI")
	})
}

func TestAdapterCancelAborts(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`)

	deadline := time.After(2 * time.Second)
	for {
		if len(h.fake.Aborts()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("cancel did not trigger an opencode abort")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mustLine(m acp.Message) string { b, _ := json.Marshal(m); return string(b) }
func itoa(i int) string             { b, _ := json.Marshal(i); return string(b) }
