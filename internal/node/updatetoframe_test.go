package node

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestUpdateToFrame covers the ACP session/update -> Frame translation, focusing on the cat A/I
// enrichment (tool content blocks + rawInput/rawOutput) while pinning the existing scalar behavior.
func TestUpdateToFrame(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   Frame
		ok     bool
	}{
		{
			name:   "agent text chunk",
			params: `{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}`,
			want:   Frame{Kind: "agent", Text: "hi"},
			ok:     true,
		},
		{
			name:   "user text chunk",
			params: `{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"yo"}}}`,
			want:   Frame{Kind: "user", Text: "yo"},
			ok:     true,
		},
		{
			name:   "thought chunk",
			params: `{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hmm"}}}`,
			want:   Frame{Kind: "thought", Text: "hmm"},
			ok:     true,
		},
		{
			name:   "tool_call creation (title+status only, no payload)",
			params: `{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","status":"pending"}}`,
			want:   Frame{Kind: "tool", ToolID: "c1", Title: "bash", Status: "pending"},
			ok:     true,
		},
		{
			name:   "tool_call creation with rawInput",
			params: `{"update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","status":"pending","rawInput":{"command":"ls"}}}`,
			want: Frame{Kind: "tool", ToolID: "c1", Title: "bash", Status: "pending",
				Tool: &ToolPayload{RawInput: json.RawMessage(`{"command":"ls"}`)}},
			ok: true,
		},
		{
			name: "tool_call_update with content + raw I/O",
			params: `{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed",` +
				`"content":[{"type":"content","content":{"type":"text","text":"file body"}}],` +
				`"rawInput":{"path":"a.go"},"rawOutput":"file body"}}`,
			want: Frame{Kind: "tool", ToolID: "c1", Status: "completed", Tool: &ToolPayload{
				Content:   []ContentBlock{{Type: "text", Text: "file body"}},
				RawInput:  json.RawMessage(`{"path":"a.go"}`),
				RawOutput: json.RawMessage(`"file body"`),
			}},
			ok: true,
		},
		{
			name:   "tool_call_update status only (no payload -> Tool nil)",
			params: `{"update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"in_progress"}}`,
			want:   Frame{Kind: "tool", ToolID: "c1", Status: "in_progress"},
			ok:     true,
		},
		{
			name:   "unknown update kind dropped",
			params: `{"update":{"sessionUpdate":"available_commands_update"}}`,
			want:   Frame{},
			ok:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := updateToFrame(json.RawMessage(tc.params))
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("frame mismatch:\n got  %+v (tool %+v)\n want %+v (tool %+v)", got, got.Tool, tc.want, tc.want.Tool)
			}
		})
	}
}
