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
	a := New(opencode.New(fake.URL), "/app", "", "")
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
	return func(m acp.Message, _ string) bool { id, ok := m.ID.AsInt(); return ok && id == n }
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
	for _, k := range []string{"allow_once", "allow_always", "reject_once"} {
		if !strings.Contains(line, k) {
			t.Fatalf("missing option kind %q in %s", k, line)
		}
	}
	h.send(`{"jsonrpc":"2.0","id":` + string(*req.ID) + `,"result":{"outcome":{"outcome":"selected","optionId":"allow_once"}}}`)

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

func TestAdapterEmitsToolCallAndUpdate(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	time.Sleep(150 * time.Millisecond) // let the pump subscribe

	// First snapshot: pending -> a tool_call creation.
	h.fake.EmitToolPart("ses_fake1", "call_1", "bash", "pending", `{"command":"ls"}`, "")
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, `"tool_call"`) && strings.Contains(line, "call_1") &&
			strings.Contains(line, `"execute"`) && strings.Contains(line, `"pending"`)
	})

	// Completed snapshot: a tool_call_update with content + rawInput/rawOutput.
	h.fake.EmitToolPart("ses_fake1", "call_1", "bash", "completed", `{"command":"ls"}`, "file1 file2")
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "tool_call_update") && strings.Contains(line, `"completed"`) &&
			strings.Contains(line, "file1 file2") && strings.Contains(line, `"rawInput":{"command":"ls"}`) &&
			strings.Contains(line, `"rawOutput":"file1 file2"`)
	})
}

func TestAdapterEmitsDiffForEditTool(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	time.Sleep(150 * time.Millisecond) // let the pump subscribe

	// A completed edit tool: the tool_call_update must carry a diff block built from the input args.
	h.fake.EmitToolPart("ses_fake1", "call_e", "edit", "completed",
		`{"filePath":"a.go","oldString":"foo","newString":"bar"}`, "")
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, "tool_call_update") && strings.Contains(line, `"type":"diff"`) &&
			strings.Contains(line, `"path":"a.go"`) && strings.Contains(line, `"oldText":"foo"`) &&
			strings.Contains(line, `"newText":"bar"`)
	})
}

func TestAdapterEmitsPlanFromTodoUpdated(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	time.Sleep(150 * time.Millisecond) // let the pump subscribe to /event

	// An opencode todo.updated event must be forwarded to the node as one ACP `plan` session/update
	// carrying the full entry list (content + mapped status/priority).
	h.fake.EmitTodoUpdated("ses_fake1", `[`+
		`{"id":"1","content":"design the api","status":"completed","priority":"high"},`+
		`{"id":"2","content":"write the code","status":"in_progress","priority":"medium"}]`)
	h.await(t, func(_ acp.Message, line string) bool {
		return strings.Contains(line, `"sessionUpdate":"plan"`) &&
			strings.Contains(line, "design the api") && strings.Contains(line, `"status":"completed"`) &&
			strings.Contains(line, "write the code") && strings.Contains(line, `"status":"in_progress"`)
	})
}

// After session/new the adapter must emit an ACP available_commands_update carrying opencode's
// advertised slash commands, so the web can build a `/`-autocomplete menu (cat E).
func TestAdapterEmitsAvailableCommands(t *testing.T) {
	h := newHarness(t)
	h.fake.SetCommands([]opencode.Command{
		{Name: "init", Description: "guided setup", Hints: []string{"$ARGUMENTS"}, Source: "command"},
		{Name: "review", Description: "review changes", Source: "command"},
	})
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	h.await(t, func(m acp.Message, line string) bool {
		return m.Method == "session/update" &&
			strings.Contains(line, `"sessionUpdate":"available_commands_update"`) &&
			strings.Contains(line, `"name":"init"`) && strings.Contains(line, `"hint":"$ARGUMENTS"`) &&
			strings.Contains(line, `"name":"review"`)
	})
}

// An agent advertising NO commands must emit no available_commands_update (graceful absence): a prompt
// turn still completes normally and never produces a commands notification.
func TestAdapterOmitsCommandsWhenNone(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"hi"}]}}`)
	// The turn-end response arrives; no available_commands_update should have been sent before it.
	m := h.await(t, func(m acp.Message, line string) bool {
		if strings.Contains(line, "available_commands_update") {
			t.Fatalf("no commands advertised -> no available_commands_update, got %s", line)
		}
		id, ok := m.ID.AsInt()
		return ok && id == 3
	})
	_ = m
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

// A turn that opencode ends with a NamedError must produce an honest stopReason (not end_turn) and a
// structured error on the session/prompt response (cat G).
func TestAdapterPromptStopReasonOnError(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	// Auth error -> end_turn stopReason BUT a structured error carrying the message.
	h.fake.ScriptTurnError(`{"name":"ProviderAuthError","data":{"providerID":"anthropic","message":"missing api key"}}`)
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"hi"}]}}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if !strings.Contains(res, `"stopReason":"end_turn"`) || !strings.Contains(res, "missing api key") {
		t.Fatalf("expected end_turn + error message, got %s", res)
	}
	if !strings.Contains(res, `"error"`) {
		t.Fatalf("expected a structured error object, got %s", res)
	}
}

// An aborted turn maps to the `cancelled` stopReason and carries NO error object.
func TestAdapterPromptCancelledStopReason(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	h.fake.ScriptTurnError(`{"name":"MessageAbortedError","data":{}}`)
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"go"}]}}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if !strings.Contains(res, `"stopReason":"cancelled"`) {
		t.Fatalf("expected cancelled stopReason, got %s", res)
	}
	if strings.Contains(res, `"error"`) {
		t.Fatalf("cancelled must not carry an error object, got %s", res)
	}
}

// A turn whose opencode steps report token usage + cost must attach a `usage` object to the
// session/prompt response (PromptResponse.usage), accumulated across all step-finish parts (cat D).
func TestAdapterPromptCarriesUsage(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	// Two LLM steps in the turn -> the usage object sums their tokens + cost.
	h.fake.ScriptStepFinish("step_1", 0.01, `{"input":100,"output":50,"reasoning":10,"cache":{"read":20,"write":0}}`)
	h.fake.ScriptStepFinish("step_2", 0.03, `{"input":200,"output":80,"reasoning":0,"cache":{"read":0,"write":0}}`)
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"hi"}]}}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if !strings.Contains(res, `"stopReason":"end_turn"`) {
		t.Fatalf("expected end_turn, got %s", res)
	}
	var r struct {
		Usage *ACPUsage `json:"usage"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil {
		t.Fatalf("expected a usage object on the prompt response, got %s", res)
	}
	if r.Usage.Input != 300 || r.Usage.Output != 130 || r.Usage.Total != 430 {
		t.Fatalf("usage tokens = %+v, want input 300 / output 130 / total 430", r.Usage)
	}
	if r.Usage.Cached != 20 || r.Usage.Thought != 10 {
		t.Fatalf("usage cached/thought = %d/%d, want 20/10", r.Usage.Cached, r.Usage.Thought)
	}
	if r.Usage.Cost == nil || *r.Usage.Cost < 0.0399 || *r.Usage.Cost > 0.0401 {
		t.Fatalf("usage cost = %v, want ~0.04", r.Usage.Cost)
	}
}

// A turn with NO step-finish parts (e.g. an agent that reports no usage) must omit the usage field, so a
// plain end_turn response is byte-stable with the pre-cat-D shape.
func TestAdapterPromptOmitsUsageWhenAbsent(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"hi"}]}}`)
	m := h.await(t, idIs(3))
	res := string(m.Result)
	if strings.Contains(res, "usage") {
		t.Fatalf("a turn with no step-finish parts must omit usage, got %s", res)
	}
}

// After session/new the adapter must advertise opencode's selectable primary agents as ACP session
// modes (currentModeId + availableModes), so the web can render a mode selector (cat F).
func TestAdapterAdvertisesModes(t *testing.T) {
	h := newHarness(t)
	h.fake.SetAgents([]opencode.Agent{
		{Name: "build", Description: "The default agent.", Mode: "primary"},
		{Name: "plan", Description: "Plan mode.", Mode: "primary"},
		{Name: "title", Mode: "primary", Hidden: true},
		{Name: "general", Mode: "subagent"},
	})
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	m := h.await(t, idIs(2))
	res := string(m.Result)
	for _, want := range []string{`"currentModeId":"build"`, `"availableModes"`, `"id":"build"`, `"name":"Build"`, `"id":"plan"`, `"name":"Plan"`} {
		if !strings.Contains(res, want) {
			t.Fatalf("session/new result missing %q: %s", want, res)
		}
	}
	if strings.Contains(res, "title") || strings.Contains(res, "general") {
		t.Fatalf("hidden/subagent agents must not be advertised as modes: %s", res)
	}
}

// An agent exposing no selectable primary agents must omit the modes block (graceful absence): the
// session/new result is the plain {sessionId} shape.
func TestAdapterOmitsModesWhenNone(t *testing.T) {
	h := newHarness(t)
	// No agents seeded -> GET /agent returns [] -> no modes.
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	m := h.await(t, idIs(2))
	res := string(m.Result)
	if strings.Contains(res, "modes") || strings.Contains(res, "availableModes") {
		t.Fatalf("no selectable agents -> no modes block, got %s", res)
	}
}

// An incoming session/set_mode must switch opencode's active agent (passed on the next prompt_async)
// and emit a current_mode_update so all clients follow the switch (cat F).
func TestAdapterSetModeSwitchesAgentAndEmitsUpdate(t *testing.T) {
	h := newHarness(t)
	h.fake.SetAgents([]opencode.Agent{
		{Name: "build", Mode: "primary"},
		{Name: "plan", Mode: "primary"},
	})
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	// Switch to plan mode.
	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/set_mode","params":{"sessionId":"ses_fake1","modeId":"plan"}}`)
	// The set_mode response is an empty object...
	h.await(t, idIs(3))
	// ...and a current_mode_update announces the switch.
	h.await(t, func(m acp.Message, line string) bool {
		return m.Method == "session/update" &&
			strings.Contains(line, `"sessionUpdate":"current_mode_update"`) &&
			strings.Contains(line, `"currentModeId":"plan"`)
	})

	// The next prompt must carry agent="plan".
	h.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"go"}]}}`)
	h.await(t, idIs(4))
	deadline := time.After(2 * time.Second)
	for {
		if pa := h.fake.PromptAgents(); len(pa) == 1 {
			if pa[0] != "plan" {
				t.Fatalf("prompt agent = %q, want plan", pa[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("prompt_async never recorded the switched agent")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// An unknown mode id must be rejected (error response) and must NOT change the active agent.
func TestAdapterSetModeRejectsUnknown(t *testing.T) {
	h := newHarness(t)
	h.fake.SetAgents([]opencode.Agent{{Name: "build", Mode: "primary"}, {Name: "plan", Mode: "primary"}})
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	h.await(t, idIs(1))
	h.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/app"}}`)
	h.await(t, idIs(2))

	h.send(`{"jsonrpc":"2.0","id":3,"method":"session/set_mode","params":{"sessionId":"ses_fake1","modeId":"bogus"}}`)
	m := h.await(t, idIs(3))
	if m.Error == nil {
		t.Fatalf("unknown mode should return an error, got result %s", string(m.Result))
	}
	// The active agent stays the default (build): a subsequent prompt carries agent="build".
	h.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"ses_fake1","prompt":[{"type":"text","text":"go"}]}}`)
	h.await(t, idIs(4))
	deadline := time.After(2 * time.Second)
	for {
		if pa := h.fake.PromptAgents(); len(pa) == 1 {
			if pa[0] != "build" {
				t.Fatalf("after a rejected set_mode the agent should stay build, got %q", pa[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("prompt_async never recorded")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
