package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// maxHistoryItems caps the in-memory transcript. Past the cap the oldest items are dropped and a
// single leading "truncated" marker is kept. On-demand pagination for long histories is a post-demo
// epic (sp-suc).
const maxHistoryItems = 500

// item is one transcript entry. It marshals directly into the spawn/history frame. Roles:
// user | agent | thought | tool | system (system = the truncation marker).
type item struct {
	Role   string `json:"role"`
	Text   string `json:"text,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// recorder accumulates a coalesced transcript from the ACP traffic flowing through the adapter.
// All methods are safe for concurrent use (the agent-side pump and the client-side copy both call in).
type recorder struct {
	mu      sync.Mutex
	items   []item
	toolIdx map[string]int // toolCallId -> index in items, for tool_call_update
}

func newRecorder() *recorder { return &recorder{toolIdx: map[string]int{}} }

// observeClient records a client->agent line if it is a session/prompt (one user item per prompt).
func (r *recorder) observeClient(line []byte) {
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
	r.push(item{Role: "user", Text: sb.String()})
}

// observeAgent records an agent->client line if it is a session/update notification.
func (r *recorder) observeAgent(line []byte) {
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
		r.push(item{Role: "tool", Title: u.Title, Status: u.Status})
		if u.ToolCallID != "" {
			r.toolIdx[u.ToolCallID] = len(r.items) - 1
		}
	case "tool_call_update":
		if i, ok := r.toolIdx[u.ToolCallID]; ok && i >= 0 && i < len(r.items) {
			r.items[i].Status = u.Status
		}
	}
}

// appendChunk coalesces consecutive same-role chunks into one item (mirrors the web client's
// appendChunk). Caller holds r.mu.
func (r *recorder) appendChunk(role, text string) {
	if text == "" {
		return
	}
	if n := len(r.items); n > 0 && r.items[n-1].Role == role {
		r.items[n-1].Text += text
		return
	}
	r.push(item{Role: role, Text: text})
}

// push appends an item and enforces the cap. Caller holds r.mu.
func (r *recorder) push(it item) {
	r.items = append(r.items, it)
	if len(r.items) <= maxHistoryItems {
		return
	}
	// Drop oldest, keep a single leading truncation marker. tool index positions are invalidated by
	// the slice, so reset it (a tool_call_update for a pre-truncation tool simply won't apply — an
	// acceptable edge at 500+ items; pagination is the post-demo epic).
	over := len(r.items) - maxHistoryItems
	trimmed := append([]item{{Role: "system", Text: "earlier history truncated"}}, r.items[over+1:]...)
	r.items = trimmed
	r.toolIdx = map[string]int{}
}

// historyFrame returns a newline-terminated spawn/history JSON-RPC notification snapshotting the
// current transcript, or nil if the transcript is empty (nothing to replay).
func (r *recorder) historyFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]item, len(r.items))
	copy(snap, r.items)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []item `json:"items"`
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
