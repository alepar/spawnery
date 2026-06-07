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
		if o.OptionID == "" || o.Name == "" || o.Kind == "" {
			t.Fatalf("option missing optionId/name/kind: %+v", o)
		}
		kinds[o.Kind] = true
	}
	// opencode's real model is once|always|reject — a persistent ALLOW but NO persistent reject — so the
	// adapter offers exactly these three honestly-kinded options (no fabricated reject_always).
	for _, want := range []string{"allow_once", "allow_always", "reject_once"} {
		if !kinds[want] {
			t.Fatalf("missing ACP option kind %q", want)
		}
	}
	if kinds["reject_always"] {
		t.Fatal("reject_always must NOT be offered: opencode has no persistent reject (it would collapse to reject)")
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

	t.Run("edit tool emits a diff content block from oldString/newString", func(t *testing.T) {
		ups := ToolPartUpdates(part("c6", "edit", "completed", `{"filePath":"a.go","oldString":"foo","newString":"bar"}`, "", "", "Edit a.go"), true)
		if len(ups) != 2 {
			t.Fatalf("want 2 updates, got %d: %+v", len(ups), ups)
		}
		if ups[0].Update.Kind != "edit" {
			t.Fatalf("creation kind = %q want edit", ups[0].Update.Kind)
		}
		u := ups[1].Update
		var diff *ACPToolContent
		for i := range u.Content {
			if u.Content[i].Type == "diff" {
				diff = &u.Content[i]
			}
		}
		if diff == nil {
			t.Fatalf("no diff block in %+v", u.Content)
		}
		if diff.Path != "a.go" || diff.OldText != "foo" || diff.NewText != "bar" {
			t.Fatalf("bad diff block: %+v", diff)
		}
	})

	t.Run("write tool emits a diff block with empty oldText and full content as newText", func(t *testing.T) {
		ups := ToolPartUpdates(part("c7", "write", "completed", `{"filePath":"new.go","content":"package x"}`, "", "", ""), false)
		if len(ups) != 1 {
			t.Fatalf("want 1 update, got %d", len(ups))
		}
		var diff *ACPToolContent
		for i := range ups[0].Update.Content {
			if ups[0].Update.Content[i].Type == "diff" {
				diff = &ups[0].Update.Content[i]
			}
		}
		if diff == nil || diff.Path != "new.go" || diff.OldText != "" || diff.NewText != "package x" {
			t.Fatalf("bad write diff block: %+v", diff)
		}
	})

	t.Run("non-edit tool emits no diff block", func(t *testing.T) {
		ups := ToolPartUpdates(part("c8", "bash", "completed", `{"command":"ls","filePath":"x"}`, "out", "", ""), false)
		for _, c := range ups[0].Update.Content {
			if c.Type == "diff" {
				t.Fatalf("bash should not emit a diff block: %+v", ups[0].Update.Content)
			}
		}
	})

	t.Run("edit with no filePath yields no diff block", func(t *testing.T) {
		if d := toolDiffBlock("edit", ToolState{Input: json.RawMessage(`{"oldString":"a","newString":"b"}`)}); d != nil {
			t.Fatalf("want nil diff without filePath, got %+v", d)
		}
	})

	t.Run("diff block serializes to canonical ACP JSON", func(t *testing.T) {
		ups := ToolPartUpdates(part("c9", "edit", "completed", `{"filePath":"a.go","oldString":"foo","newString":"bar"}`, "", "", ""), false)
		b, _ := json.Marshal(ups[0])
		s := string(b)
		for _, want := range []string{`"type":"diff"`, `"path":"a.go"`, `"oldText":"foo"`, `"newText":"bar"`} {
			if !contains(s, want) {
				t.Fatalf("missing %q in %s", want, s)
			}
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

func TestStopReasonForError(t *testing.T) {
	mk := func(name, msg string) *OpencodeError {
		e := &OpencodeError{Name: name}
		e.Data.Message = msg
		return e
	}
	cases := []struct {
		name     string
		err      *OpencodeError
		wantStop string
		wantErr  bool   // expect a structured error object
		wantMsg  string // when wantErr, the message (substring) expected
	}{
		{"nil -> clean end_turn", nil, "end_turn", false, ""},
		{"empty name -> end_turn", &OpencodeError{}, "end_turn", false, ""},
		{"aborted -> cancelled, no error", mk("MessageAbortedError", ""), "cancelled", false, ""},
		{"output length -> max_tokens, no error", mk("MessageOutputLengthError", ""), "max_tokens", false, ""},
		{"context overflow -> max_tokens + error", mk("ContextOverflowError", ""), "max_tokens", true, "context"},
		{"refusal -> refusal + error", mk("RefusalError", "I won't"), "refusal", true, "I won't"},
		{"auth -> end_turn + error message", mk("ProviderAuthError", "missing api key"), "end_turn", true, "missing api key"},
		{"unknown w/o message -> end_turn + error named", mk("UnknownError", ""), "end_turn", true, "UnknownError"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stop, ei := StopReasonForError(c.err)
			if stop != c.wantStop {
				t.Fatalf("stop = %q, want %q", stop, c.wantStop)
			}
			if c.wantErr {
				if ei == nil {
					t.Fatalf("want structured error, got nil")
				}
				if c.wantMsg != "" && !contains(ei.Message, c.wantMsg) {
					t.Fatalf("error message %q missing %q", ei.Message, c.wantMsg)
				}
			} else if ei != nil {
				t.Fatalf("want no error, got %+v", ei)
			}
		})
	}
}

func TestParseSessionError(t *testing.T) {
	se, err := ParseSessionError(json.RawMessage(`{"sessionID":"ses_1","error":{"name":"ProviderAuthError","data":{"providerID":"anthropic","message":"bad key"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if se.Error == nil || se.Error.Name != "ProviderAuthError" || se.Error.Data.Message != "bad key" {
		t.Fatalf("bad parse: %+v", se)
	}
}
