package ocadapter

import (
	"encoding/json"
	"testing"
)

func TestParsePartUpdatedAndKind(t *testing.T) {
	raw := json.RawMessage(`{"sessionID":"ses_1","part":{"id":"prt_1","type":"text","text":"hello"},"time":1}`)
	pu, err := ParsePartUpdated(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pu.SessionID != "ses_1" || pu.Part.ID != "prt_1" || pu.Part.Type != "text" {
		t.Fatalf("bad parse: %+v", pu)
	}
	kind, ok := PartTypeToACPKind(pu.Part.Type)
	if !ok || kind != "agent_message_chunk" {
		t.Fatalf("text should map to agent_message_chunk, got %q ok=%v", kind, ok)
	}
}

func TestReasoningMapsToThought(t *testing.T) {
	kind, ok := PartTypeToACPKind("reasoning")
	if !ok || kind != "agent_thought_chunk" {
		t.Fatalf("reasoning -> agent_thought_chunk, got %q ok=%v", kind, ok)
	}
	if _, ok := PartTypeToACPKind("tool"); ok {
		t.Fatal("tool should not map to a text chunk")
	}
}

func TestParsePartDelta(t *testing.T) {
	raw := json.RawMessage(`{"sessionID":"ses_1","messageID":"msg_1","partID":"prt_1","field":"text","delta":"lo"}`)
	d, err := ParsePartDelta(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.PartID != "prt_1" || d.Field != "text" || d.Delta != "lo" {
		t.Fatalf("bad delta parse: %+v", d)
	}
}

func TestACPSessionUpdateShape(t *testing.T) {
	u := ACPSessionUpdate("ses_1", "agent_message_chunk", "hi")
	b, _ := json.Marshal(u)
	s := string(b)
	if !contains(s, `"sessionId":"ses_1"`) || !contains(s, `"sessionUpdate":"agent_message_chunk"`) || !contains(s, `"text":"hi"`) {
		t.Fatalf("bad ACP update json: %s", s)
	}
}

func TestPermissionOptionsAndMapping(t *testing.T) {
	opts := PermissionToACPOptions()
	kinds := map[string]bool{}
	for _, o := range opts {
		kinds[o.Kind] = true
	}
	for _, want := range []string{"allow_once", "allow_always", "reject_once", "reject_always"} {
		if !kinds[want] {
			t.Fatalf("missing ACP option kind %q", want)
		}
	}
	if ACPOptionIDToOpencodeResponse("allow_once") != "once" ||
		ACPOptionIDToOpencodeResponse("allow_always") != "always" ||
		ACPOptionIDToOpencodeResponse("reject_once") != "reject" {
		t.Fatal("optionId -> opencode response mapping wrong")
	}
}

func TestToolStatusAndKindMapping(t *testing.T) {
	for in, want := range map[string]string{"pending": "pending", "running": "in_progress", "completed": "completed", "error": "failed", "": "pending"} {
		if got := ToolStatusToACP(in); got != want {
			t.Fatalf("ToolStatusToACP(%q)=%q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"read": "read", "edit": "edit", "bash": "execute", "grep": "search", "webfetch": "fetch", "weirdtool": "other"} {
		if got := ToolKind(in); got != want {
			t.Fatalf("ToolKind(%q)=%q want %q", in, got, want)
		}
	}
}

func TestToolPartUpdates(t *testing.T) {
	// helper to build a PartUpdated tool snapshot
	part := func(callID, tool, status, input, output, errMsg, title string) PartUpdated {
		var pu PartUpdated
		pu.SessionID = "ses_1"
		pu.Part.Type = "tool"
		pu.Part.CallID = callID
		pu.Part.Tool = tool
		pu.Part.State = ToolState{Status: status, Title: title, Output: output, Error: errMsg}
		if input != "" {
			pu.Part.State.Input = json.RawMessage(input)
		}
		return pu
	}

	t.Run("first pending emits only a tool_call creation", func(t *testing.T) {
		ups := ToolPartUpdates(part("c1", "bash", "pending", `{"command":"ls"}`, "", "", ""), true)
		if len(ups) != 1 {
			t.Fatalf("want 1 update, got %d: %+v", len(ups), ups)
		}
		u := ups[0].Update
		if u.SessionUpdate != "tool_call" || u.ToolCallID != "c1" || u.Title != "bash" || u.Kind != "execute" || u.Status != "pending" {
			t.Fatalf("bad creation: %+v", u)
		}
	})

	t.Run("first completed emits creation + completed update with content + raw", func(t *testing.T) {
		ups := ToolPartUpdates(part("c2", "read", "completed", `{"path":"a.go"}`, "file body", "", "Read a.go"), true)
		if len(ups) != 2 {
			t.Fatalf("want 2 updates, got %d: %+v", len(ups), ups)
		}
		if ups[0].Update.SessionUpdate != "tool_call" || ups[0].Update.Title != "Read a.go" || ups[0].Update.Kind != "read" {
			t.Fatalf("bad creation: %+v", ups[0].Update)
		}
		u := ups[1].Update
		if u.SessionUpdate != "tool_call_update" || u.ToolCallID != "c2" || u.Status != "completed" {
			t.Fatalf("bad update: %+v", u)
		}
		if len(u.Content) != 1 || u.Content[0].Type != "content" || u.Content[0].Content.Type != "text" || u.Content[0].Content.Text != "file body" {
			t.Fatalf("bad content block: %+v", u.Content)
		}
		if string(u.RawInput) != `{"path":"a.go"}` {
			t.Fatalf("bad rawInput: %s", string(u.RawInput))
		}
		if string(u.RawOutput) != `"file body"` {
			t.Fatalf("bad rawOutput: %s", string(u.RawOutput))
		}
	})

	t.Run("non-first running emits only an in_progress update", func(t *testing.T) {
		ups := ToolPartUpdates(part("c3", "bash", "running", `{"command":"ls"}`, "", "", ""), false)
		if len(ups) != 1 || ups[0].Update.SessionUpdate != "tool_call_update" || ups[0].Update.Status != "in_progress" {
			t.Fatalf("bad running update: %+v", ups)
		}
	})

	t.Run("error maps to failed with the error text", func(t *testing.T) {
		ups := ToolPartUpdates(part("c4", "bash", "error", `{"command":"boom"}`, "", "command failed", ""), false)
		if len(ups) != 1 {
			t.Fatalf("want 1, got %d", len(ups))
		}
		u := ups[0].Update
		if u.Status != "failed" || u.Content[0].Content.Text != "command failed" || string(u.RawOutput) != `"command failed"` {
			t.Fatalf("bad error update: %+v", u)
		}
	})

	t.Run("no callID yields nothing", func(t *testing.T) {
		if ups := ToolPartUpdates(part("", "bash", "completed", "", "x", "", ""), true); ups != nil {
			t.Fatalf("want nil for empty callID, got %+v", ups)
		}
	})

	t.Run("creation update serializes to canonical ACP JSON", func(t *testing.T) {
		b, _ := json.Marshal(ToolPartUpdates(part("c5", "bash", "completed", `{"command":"ls"}`, "ok", "", ""), true)[1])
		s := string(b)
		for _, want := range []string{`"sessionUpdate":"tool_call_update"`, `"toolCallId":"c5"`, `"status":"completed"`, `"rawInput":{"command":"ls"}`, `"rawOutput":"ok"`, `"type":"content"`} {
			if !contains(s, want) {
				t.Fatalf("missing %q in %s", want, s)
			}
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
