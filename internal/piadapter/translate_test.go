package piadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPromptCommandShape(t *testing.T) {
	b, err := json.Marshal(PromptCommand("hi there"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"type":"prompt"`) || !strings.Contains(s, `"message":"hi there"`) {
		t.Fatalf("prompt command shape: %s", s)
	}
}

func TestAbortCommandShape(t *testing.T) {
	b, _ := json.Marshal(AbortCommand())
	if string(b) != `{"type":"abort"}` {
		t.Fatalf("abort command shape: %s", string(b))
	}
}

func TestToolKind(t *testing.T) {
	cases := map[string]string{
		"read": "read", "list": "read", "glob": "read",
		"write": "edit", "edit": "edit", "patch": "edit",
		"bash": "execute", "shell": "execute",
		"grep": "search", "search": "search",
		"webfetch": "fetch", "fetch": "fetch",
		"something_else": "other", "": "other",
	}
	for tool, want := range cases {
		if got := ToolKind(tool); got != want {
			t.Errorf("ToolKind(%q)=%q want %q", tool, got, want)
		}
	}
}

func TestStopReasonForError(t *testing.T) {
	if stop, ei := StopReasonForError(nil); stop != "end_turn" || ei != nil {
		t.Fatalf("nil error => end_turn/no-error, got %q/%v", stop, ei)
	}
	if stop, ei := StopReasonForError(&PiError{Name: "MessageAborted"}); stop != "cancelled" || ei != nil {
		t.Fatalf("abort => cancelled/no-error, got %q/%v", stop, ei)
	}
	if stop, ei := StopReasonForError(&PiError{Name: "OutputLengthExceeded"}); stop != "max_tokens" || ei != nil {
		t.Fatalf("output-length => max_tokens/no-error, got %q/%v", stop, ei)
	}
	stop, ei := StopReasonForError(&PiError{Name: "ProviderAuthError", Message: "missing api key"})
	if stop != "end_turn" || ei == nil || !strings.Contains(ei.Message, "missing api key") {
		t.Fatalf("auth => end_turn + error msg, got %q/%v", stop, ei)
	}
}

func TestStopReasonEmptyName(t *testing.T) {
	stop, ei := StopReasonForError(&PiError{})
	if stop != "end_turn" || ei != nil {
		t.Fatalf("empty name => end_turn/nil, got %q/%v", stop, ei)
	}
}

func TestPiUsageToACP(t *testing.T) {
	u := PiUsageToACP(&PiUsage{Input: 100, Output: 50, Cached: 20, Reasoning: 10})
	if u == nil || u.Input != 100 || u.Output != 50 || u.Cached != 20 || u.Thought != 10 || u.Total != 150 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestPiUsageToACPNilOnZero(t *testing.T) {
	if u := PiUsageToACP(nil); u != nil {
		t.Fatalf("nil input should yield nil, got %+v", u)
	}
	if u := PiUsageToACP(&PiUsage{}); u != nil {
		t.Fatalf("all-zero should yield nil, got %+v", u)
	}
}

func TestACPSessionUpdateShape(t *testing.T) {
	u := ACPSessionUpdate("ses_1", "agent_message_chunk", "hi")
	b, _ := json.Marshal(u)
	s := string(b)
	if !strings.Contains(s, `"sessionId":"ses_1"`) ||
		!strings.Contains(s, `"sessionUpdate":"agent_message_chunk"`) ||
		!strings.Contains(s, `"text":"hi"`) {
		t.Fatalf("bad ACP update json: %s", s)
	}
}

func TestACPToolCallShape(t *testing.T) {
	u := ACPToolCall("ses_1", "tc1", "bash", "execute")
	b, _ := json.Marshal(u)
	s := string(b)
	for _, want := range []string{
		`"sessionId":"ses_1"`,
		`"sessionUpdate":"tool_call"`,
		`"toolCallId":"tc1"`,
		`"title":"bash"`,
		`"kind":"execute"`,
		`"status":"pending"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in tool_call shape: %s", want, s)
		}
	}
}

func TestACPToolCallUpdateShape(t *testing.T) {
	u := ACPToolCallUpdate("ses_1", "tc1", "file1 file2", false, json.RawMessage(`{"command":"ls"}`))
	b, _ := json.Marshal(u)
	s := string(b)
	for _, want := range []string{
		`"sessionUpdate":"tool_call_update"`,
		`"toolCallId":"tc1"`,
		`"status":"completed"`,
		`"file1 file2"`,
		`"rawInput":{"command":"ls"}`,
		`"rawOutput":"file1 file2"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in tool_call_update shape: %s", want, s)
		}
	}
}

func TestACPToolCallUpdateFailed(t *testing.T) {
	u := ACPToolCallUpdate("ses_1", "tc2", "command failed", true, nil)
	b, _ := json.Marshal(u)
	s := string(b)
	if !strings.Contains(s, `"status":"failed"`) || !strings.Contains(s, "command failed") {
		t.Fatalf("unexpected failed update: %s", s)
	}
}

func TestEventParsing(t *testing.T) {
	raw := `{"type":"message_update","delta":"hello","channel":""}`
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "message_update" || e.Delta != "hello" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestEventToolExecutionParsing(t *testing.T) {
	raw := `{"type":"tool_execution_start","toolCallId":"tc1","name":"bash","input":{"command":"ls"}}`
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "tool_execution_start" || e.ToolCallID != "tc1" || e.Name != "bash" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if string(e.Input) != `{"command":"ls"}` {
		t.Fatalf("unexpected input: %s", string(e.Input))
	}
}
