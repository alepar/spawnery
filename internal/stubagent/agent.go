// Package stubagent is a deterministic ACP test double for the spawnlet slice.
// It speaks the real Agent Client Protocol (JSON-RPC 2.0, ndjson framing) just
// far enough to answer initialize, session/new, and session/prompt: for a
// prompt it emits one streamed session/update agent_message_chunk echoing the
// prompt text, then a final result with a stopReason. It makes no network calls.
package stubagent

import (
	"encoding/json"
	"io"

	"spawnery/internal/acp"
)

// Run reads ACP messages from r and writes responses to w until EOF.
func Run(r io.Reader, w io.Writer) error {
	rd := acp.NewReader(r)
	for {
		msg, err := rd.ReadMessage()
		if err != nil {
			return err
		}
		switch msg.Method {
		case "initialize":
			reply(w, msg.ID, json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}`))
		case "session/new":
			reply(w, msg.ID, json.RawMessage(`{"sessionId":"stub-1"}`))
		case "session/prompt":
			text := promptText(msg.Params)
			// streamed real-ACP update notification (no id).
			upd, _ := json.Marshal(map[string]any{
				"sessionId": "stub-1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "ECHO: " + text},
				},
			})
			acp.WriteMessage(w, acp.Message{Method: "session/update", Params: upd})
			reply(w, msg.ID, json.RawMessage(`{"stopReason":"end_turn"}`))
		}
	}
}

// promptText extracts the concatenated text from a real-ACP session/prompt
// params object ({"sessionId":..,"prompt":[{"type":"text","text":..}]}).
func promptText(params json.RawMessage) string {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	var out string
	for _, b := range p.Prompt {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

func reply(w io.Writer, id *int, result json.RawMessage) {
	acp.WriteMessage(w, acp.Message{ID: id, Result: result})
}
