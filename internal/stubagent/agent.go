// Package stubagent is a deterministic ACP test double for the spawnlet slice.
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
			reply(w, msg.ID, json.RawMessage(`{"protocolVersion":"slice-0"}`))
		case "session/new":
			reply(w, msg.ID, json.RawMessage(`{"sessionId":"stub-1"}`))
		case "session/prompt":
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			// streamed update notification (no id)
			upd, _ := json.Marshal(map[string]string{"chunk": "ECHO: " + p.Text})
			acp.WriteMessage(w, acp.Message{Method: "session/update", Params: upd})
			reply(w, msg.ID, json.RawMessage(`{"stopReason":"end_turn"}`))
		}
	}
}

func reply(w io.Writer, id *int, result json.RawMessage) {
	acp.WriteMessage(w, acp.Message{ID: id, Result: result})
}
