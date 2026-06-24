package piadapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"spawnery/internal/acp"
)

// testHarness wires a node-side ACP pipe to an adapter backed by a fake pi
// subprocess (in-memory pipes). It mirrors the ocadapter test harness.
type testHarness struct {
	in    io.Writer      // node -> adapter (write ACP requests here)
	lines chan string     // adapter -> node (ACP responses/notifications)
	cmds  chan string     // adapter -> pi stdin (commands adapter sent to pi)
	piOut *io.PipeWriter // pi stdout write end (test emits events here)
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()

	// node <-> adapter pipes
	adapterR, nodeW := io.Pipe()
	nodeR, adapterW := io.Pipe()

	// pi <-> adapter pipes (test controls both ends)
	piStdinR, piStdinW := io.Pipe()   // adapter writes commands; test reads
	piStdoutR, piStdoutW := io.Pipe() // test writes events; adapter reads

	fakeLaunch := launchFunc(func(_, _ string) (io.WriteCloser, io.ReadCloser, func(), error) {
		return piStdinW, piStdoutR, func() {}, nil
	})

	a := New("test-model", "/app", withLaunch(fakeLaunch))
	go func() { _ = a.Serve(adapterR, adapterW) }()

	t.Cleanup(func() {
		_ = piStdoutW.Close()
		_ = piStdinR.Close()
		_ = adapterR.Close()
		_ = nodeW.Close()
		_ = adapterW.Close()
		_ = nodeR.Close()
		time.Sleep(30 * time.Millisecond)
	})

	// drain adapter -> node
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

	// drain pi stdin (commands from adapter)
	cmds := make(chan string, 64)
	go func() {
		br := bufio.NewReader(piStdinR)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				cmds <- line
			}
			if err != nil {
				return
			}
		}
	}()

	return &testHarness{
		in:    nodeW,
		lines: lines,
		cmds:  cmds,
		piOut: piStdoutW,
	}
}

// send writes an ACP message line to the adapter (node side).
func (h *testHarness) send(s string) { _, _ = io.WriteString(h.in, s+"\n") }

// emit writes a pi-rpc event line to the adapter (pi stdout side).
func (h *testHarness) emit(s string) { _, _ = io.WriteString(h.piOut, s+"\n") }

// await scans incoming ACP lines until pred matches or times out.
func (h *testHarness) await(t *testing.T, pred func(acp.Message, string) bool) acp.Message {
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

// awaitCmd scans the pi-stdin channel until a line containing sub is found.
func (h *testHarness) awaitCmd(t *testing.T, sub string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-h.cmds:
			if strings.Contains(line, sub) {
				return line
			}
		case <-deadline:
			t.Fatalf("timed out waiting for pi command containing %q", sub)
			return ""
		}
	}
}

func idIs(n int) func(acp.Message, string) bool {
	return func(m acp.Message, _ string) bool {
		id, ok := m.ID.AsInt()
		return ok && id == n
	}
}

// primeTurn drives the adapter through initialize + session/new + session/prompt
// so subsequent tests start from a mid-turn state (inflight prompt pending).
func primeTurn(t *testing.T, h *testHarness) {
	t.Helper()
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)
	h.await(t, idIs(2))
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"pi-session-1","prompt":[{"type":"text","text":"say hi"}]}}`)
	// Drain the prompt command that the adapter sent to pi stdin.
	h.awaitCmd(t, `"type":"prompt"`)
}

// --- tests ------------------------------------------------------------------

func TestInitializeAndSessionNew(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	m := h.await(t, idIs(1))
	if m.Result == nil || !strings.Contains(string(m.Result), "protocolVersion") {
		t.Fatalf("bad initialize result: %s", string(m.Result))
	}
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)
	m = h.await(t, idIs(2))
	if !strings.Contains(string(m.Result), "sessionId") {
		t.Fatalf("session/new must return sessionId, got: %s", string(m.Result))
	}
}

func TestPromptStreamsDeltaThenTurnEnd(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`)
	h.await(t, idIs(2))
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"pi-session-1","prompt":[{"type":"text","text":"hi"}]}}`)
	h.awaitCmd(t, `"type":"prompt"`)

	// Emit message_update chunks then complete the turn.
	h.emit(`{"type":"message_update","delta":"hello ","channel":""}`)
	h.emit(`{"type":"message_update","delta":"world","channel":""}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "agent_message_chunk") && strings.Contains(line, "hello ")
	})
	h.emit(`{"type":"turn_end"}`)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	if !strings.Contains(string(m.Result), `"stopReason":"end_turn"`) {
		t.Fatalf("expected end_turn response, got %s", string(m.Result))
	}
}

func TestThinkingChannelEmitsThoughtChunk(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"message_update","delta":"thinking...","channel":"thinking"}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "agent_thought_chunk") && strings.Contains(line, "thinking...")
	})
	h.emit(`{"type":"agent_end"}`)
	h.await(t, idIs(3))
}

func TestToolCallRoundTrip(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)

	// tool_execution_start -> tool_call creation
	h.emit(`{"type":"tool_execution_start","toolCallId":"tc1","name":"bash","input":{"command":"ls"}}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, `"tool_call"`) &&
			strings.Contains(line, "tc1") &&
			strings.Contains(line, `"execute"`) &&
			strings.Contains(line, `"pending"`)
	})

	// tool_execution_end -> tool_call_update completed
	h.emit(`{"type":"tool_execution_end","toolCallId":"tc1","output":"file1 file2","isError":false}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "tool_call_update") &&
			strings.Contains(line, `"completed"`) &&
			strings.Contains(line, "file1 file2") &&
			strings.Contains(line, `"rawInput":{"command":"ls"}`)
	})

	h.emit(`{"type":"agent_end"}`)
	h.await(t, idIs(3))
}

func TestToolCallErrorMapsToFailed(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"tool_execution_start","toolCallId":"tc2","name":"bash","input":{"command":"boom"}}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, `"tool_call"`) && strings.Contains(line, "tc2")
	})
	h.emit(`{"type":"tool_execution_end","toolCallId":"tc2","output":"command not found","isError":true}`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "tool_call_update") && strings.Contains(line, `"failed"`)
	})
	h.emit(`{"type":"agent_end"}`)
	h.await(t, idIs(3))
}

func TestCancelSendsAbort(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`)
	h.awaitCmd(t, `"type":"abort"`)
}

func TestTurnUsageCarriedToResponse(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"turn_end","usage":{"inputTokens":100,"outputTokens":50,"cachedTokens":20,"reasoningTokens":10}}`)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	var r struct {
		StopReason string    `json:"stopReason"`
		Usage      *ACPUsage `json:"usage"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if r.StopReason != "end_turn" || r.Usage == nil ||
		r.Usage.Input != 100 || r.Usage.Output != 50 || r.Usage.Total != 150 ||
		r.Usage.Cached != 20 || r.Usage.Thought != 10 {
		t.Fatalf("usage not carried correctly: %s", string(m.Result))
	}
}

func TestUsageOmittedWhenAbsent(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	if strings.Contains(string(m.Result), "usage") {
		t.Fatalf("no usage reported => usage field must be absent, got %s", string(m.Result))
	}
}

func TestErrorEventSetsStopReasonAndError(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"extension_error","name":"ProviderAuthError","message":"missing api key"}`)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if !strings.Contains(res, `"stopReason":"end_turn"`) ||
		!strings.Contains(res, "missing api key") ||
		!strings.Contains(res, `"error"`) {
		t.Fatalf("expected end_turn + structured error, got %s", res)
	}
}

func TestAbortErrorMapsToCancel(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	h.emit(`{"type":"extension_error","name":"MessageAborted","message":""}`)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if !strings.Contains(res, `"stopReason":"cancelled"`) {
		t.Fatalf("expected cancelled stopReason, got %s", res)
	}
	if strings.Contains(res, `"error"`) {
		t.Fatalf("cancelled must not carry an error object, got %s", res)
	}
}

func TestPiExitEndsInflightTurn(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	// Simulate pi stdout closing (process exited) mid-turn.
	_ = h.piOut.Close()
	m := h.await(t, idIs(3))
	if !strings.Contains(string(m.Result), `"error"`) {
		t.Fatalf("pi exit mid-turn must end the turn with an error, got %s", string(m.Result))
	}
}

func TestUnknownEventsAreIgnored(t *testing.T) {
	h := newHarness(t)
	primeTurn(t, h)
	// Emit events from a future pi release that we don't know about.
	h.emit(`{"type":"some_future_event","payload":{"foo":"bar"}}`)
	h.emit(`{"type":"agent_end"}`)
	m := h.await(t, idIs(3))
	if !strings.Contains(string(m.Result), `"stopReason"`) {
		t.Fatalf("unexpected events should be silently ignored, got %s", string(m.Result))
	}
}
