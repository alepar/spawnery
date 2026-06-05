package node

import (
	"encoding/json"
	"testing"
)

// These tests pin the node's ACP consumption to the NEUTRAL spec shapes — not
// any one agent's dialect. The opencode adapter (and a future goose pass-through)
// must emit exactly these. If the node ever grows an agent-specific assumption,
// one of these breaks. See docs/superpowers/specs/2026-06-05-opencode-swap-and-terminal-design.md.

func TestUpdateToFrame_CanonicalACP(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   Frame
	}{
		{
			name:   "agent_message_chunk",
			params: `{"sessionId":"ses_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}`,
			want:   Frame{Kind: "agent", Text: "hello"},
		},
		{
			name:   "agent_thought_chunk",
			params: `{"sessionId":"ses_1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}}`,
			want:   Frame{Kind: "thought", Text: "thinking"},
		},
		{
			name:   "tool_call",
			params: `{"sessionId":"ses_1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"bash","status":"pending"}}`,
			want:   Frame{Kind: "tool", ToolID: "t1", Title: "bash", Status: "pending"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := updateToFrame(json.RawMessage(tc.params))
			if !ok {
				t.Fatalf("updateToFrame returned ok=false for canonical %s", tc.name)
			}
			if got != tc.want {
				t.Fatalf("frame mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

func TestPickPermOption_CanonicalACPKinds(t *testing.T) {
	// The four canonical ACP PermissionOption kinds, as the opencode adapter emits.
	opts := json.RawMessage(`[
		{"optionId":"allow_once","name":"Allow once","kind":"allow_once"},
		{"optionId":"allow_always","name":"Allow always","kind":"allow_always"},
		{"optionId":"reject_once","name":"Reject once","kind":"reject_once"},
		{"optionId":"reject_always","name":"Reject always","kind":"reject_always"}
	]`)

	if got := pickPermOption(opts, true); got != "allow_once" && got != "allow_always" {
		t.Fatalf("allow should pick an allow_* option, got %q", got)
	}
	if got := pickPermOption(opts, false); got != "reject_once" && got != "reject_always" {
		t.Fatalf("deny should pick a reject_* option, got %q", got)
	}
}
