// Package transcript records an ACP ndjson conversation (session/prompt + session/update) into a
// coalesced transcript and serializes it as a spawn/history JSON-RPC notification for replay to a
// (re)connecting client. Used by the in-pod acpadapter (CRI lane) and the node relay (Docker lane).
package transcript

import (
	"encoding/json"
	"strings"
	"sync"
)

// MaxItems caps the in-memory transcript. Past the cap the oldest items are dropped and a single
// leading "truncated" marker is kept. On-demand pagination for long histories is a separate epic.
const MaxItems = 500

// Item is one transcript entry. It marshals directly into the spawn/history frame. Roles:
// user | agent | thought | tool | system (system = the truncation marker).
type Item struct {
	Role   string `json:"role"`
	Text   string `json:"text,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// Recorder accumulates a coalesced transcript. All methods are safe for concurrent use.
type Recorder struct {
	mu      sync.Mutex
	items   []Item
	toolIdx map[string]int // toolCallId -> index in items, for tool_call_update
}

func New() *Recorder { return &Recorder{toolIdx: map[string]int{}} }

// ObserveClientLine records a client->agent ndjson line if it is a session/prompt (one user item).
func (r *Recorder) ObserveClientLine(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/prompt" {
		return
	}
	var sb strings.Builder
	for _, p := range m.Params.Prompt {
		sb.WriteString(p.Text)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.push(Item{Role: "user", Text: sb.String()})
}

// ObserveAgentLine records an agent->client ndjson line if it is a session/update notification.
func (r *Recorder) ObserveAgentLine(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
				ToolCallID string `json:"toolCallId"`
				Title      string `json:"title"`
				Status     string `json:"status"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return
	}
	u := m.Params.Update
	r.mu.Lock()
	defer r.mu.Unlock()
	switch u.SessionUpdate {
	case "agent_message_chunk":
		r.appendChunk("agent", u.Content.Text)
	case "agent_thought_chunk":
		r.appendChunk("thought", u.Content.Text)
	case "tool_call":
		r.push(Item{Role: "tool", Title: u.Title, Status: u.Status})
		if u.ToolCallID != "" {
			r.toolIdx[u.ToolCallID] = len(r.items) - 1
		}
	case "tool_call_update":
		if i, ok := r.toolIdx[u.ToolCallID]; ok && i >= 0 && i < len(r.items) {
			r.items[i].Status = u.Status
		}
	}
}

// appendChunk coalesces consecutive same-role chunks into one item. Caller holds r.mu.
func (r *Recorder) appendChunk(role, text string) {
	if text == "" {
		return
	}
	if n := len(r.items); n > 0 && r.items[n-1].Role == role {
		r.items[n-1].Text += text
		return
	}
	r.push(Item{Role: role, Text: text})
}

// push appends an item and enforces the cap. Caller holds r.mu.
func (r *Recorder) push(it Item) {
	r.items = append(r.items, it)
	if len(r.items) <= MaxItems {
		return
	}
	over := len(r.items) - MaxItems
	trimmed := append([]Item{{Role: "system", Text: "earlier history truncated"}}, r.items[over+1:]...)
	r.items = trimmed
	r.toolIdx = map[string]int{}
}

// HistoryFrame returns a newline-terminated spawn/history JSON-RPC notification snapshotting the
// current transcript, or nil if the transcript is empty (nothing to replay).
func (r *Recorder) HistoryFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]Item, len(r.items))
	copy(snap, r.items)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []Item `json:"items"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/history"
	env.Params.Items = snap
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
