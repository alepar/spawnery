package node

import (
	"encoding/json"
	"reflect"
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
			name:   "user_message_chunk",
			params: `{"sessionId":"ses_1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"from another client"}}}`,
			want:   Frame{Kind: "user", Text: "from another client"},
		},
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
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("frame mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

func TestRejectOptionID_CanonicalACPKinds(t *testing.T) {
	// The canonical ACP PermissionOption kinds, as the opencode adapter emits. The node forwards the
	// client's chosen optionId verbatim; rejectOptionID only chooses the auto-deny / dismissed fallback.
	opts := []PermOption{
		{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
		{OptionID: "reject_once", Name: "Reject", Kind: "reject_once"},
	}
	if got := rejectOptionID(opts); got != "reject_once" {
		t.Fatalf("auto-deny should pick the reject option, got %q", got)
	}
	// No reject-ish option -> fall back to the first; empty set -> "".
	if got := rejectOptionID([]PermOption{{OptionID: "ok", Kind: "allow"}}); got != "ok" {
		t.Fatalf("fallback should be the first option, got %q", got)
	}
	if got := rejectOptionID(nil); got != "" {
		t.Fatalf("empty options should yield \"\", got %q", got)
	}
}
