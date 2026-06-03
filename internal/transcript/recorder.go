// Package transcript records an ACP ndjson conversation and brokers per-spawn turn-state: it tracks
// the in-flight session/prompt, queues prompts received while busy, drains them FIFO on turn-end,
// and serializes the transcript + turn-state as spawn/history / spawn/turn frames. Used by the
// in-pod acpadapter (CRI lane) and the node relay (Docker lane).
package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// MaxItems caps the in-memory transcript. Past the cap the oldest items are dropped and a single
// leading "truncated" marker is kept. On-demand pagination for long histories is a separate epic.
const MaxItems = 500

// MaxQueued caps prompts buffered while the agent is busy. The web client also gates on this
// (web/src/lib/turn.ts MAX_QUEUED); over the cap the broker drops silently (defence in depth).
const MaxQueued = 50

// Item is one transcript entry. It marshals directly into the spawn/history frame. Roles:
// user | agent | thought | tool | system (system = the truncation marker).
type Item struct {
	Role    string `json:"role"`
	Text    string `json:"text,omitempty"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status,omitempty"`
	Pending bool   `json:"pending,omitempty"` // queued prompt not yet forwarded to the agent
}

// Recorder accumulates a coalesced transcript. All methods are safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	items    []Item
	toolIdx  map[string]int // toolCallId -> index in items, for tool_call_update
	busy     bool           // a session/prompt turn is in flight
	inflight *int           // JSON-RPC id of the in-flight prompt (nil if the client omitted one)
	queue    [][]byte       // raw client prompt lines held while busy, FIFO
	lastTurn string         // last (state:queued) emitted, to suppress duplicate spawn/turn frames
}

func New() *Recorder { return &Recorder{toolIdx: map[string]int{}} }

// recordUserLocked appends a user item. Caller holds r.mu.
func (r *Recorder) recordUserLocked(text string, pending bool) {
	r.push(Item{Role: "user", Text: text, Pending: pending})
}

// promptText extracts the concatenated text of a session/prompt line, or ("", false) if the line
// is not a session/prompt. The returned id is the JSON-RPC request id (nil if absent).
func promptText(line []byte) (text string, id *int, ok bool) {
	var m struct {
		Method string `json:"method"`
		ID     *int   `json:"id"`
		Params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/prompt" {
		return "", nil, false
	}
	var sb strings.Builder
	for _, p := range m.Params.Prompt {
		sb.WriteString(p.Text)
	}
	return sb.String(), m.ID, true
}

// ObserveClientLine records a client->agent ndjson line if it is a session/prompt (one user item).
// Pure record: no turn-state side effects.
func (r *Recorder) ObserveClientLine(line []byte) {
	text, _, ok := promptText(line)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordUserLocked(text, false)
}

// OnClientLine records and GATES a client->agent ndjson line. It returns the line(s) to forward to
// the agent now (a non-prompt line passes through; an idle prompt is forwarded and starts a turn; a
// prompt received while busy is held, recorded as a pending user item, and queued) and an optional
// spawn/turn notification to send to the client.
func (r *Recorder) OnClientLine(line []byte) (forward [][]byte, turn []byte) {
	text, id, isPrompt := promptText(line)
	if !isPrompt {
		return [][]byte{line}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.busy {
		r.busy = true
		r.inflight = id
		r.recordUserLocked(text, false)
		return [][]byte{line}, r.turnFrameLocked()
	}
	if len(r.queue) >= MaxQueued {
		return nil, nil
	}
	r.recordUserLocked(text, true)
	r.queue = append(r.queue, append([]byte(nil), line...))
	return nil, r.turnFrameLocked()
}

type agentUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	Content       struct {
		Text string `json:"text"`
	} `json:"content"`
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
}

// observeUpdateLocked records a session/update notification. Caller holds r.mu.
func (r *Recorder) observeUpdateLocked(u agentUpdate) {
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

// ObserveAgentLine records an agent->client ndjson line if it is a session/update notification.
// Pure record: no turn-state side effects.
func (r *Recorder) ObserveAgentLine(line []byte) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update agentUpdate `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.Method != "session/update" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeUpdateLocked(m.Params.Update)
}

// OnAgentLine records an agent->client ndjson line AND detects turn-end. It returns prompt line(s)
// to forward to the agent now (a drained queued prompt, if the turn just ended) and an optional
// spawn/turn notification for the client.
func (r *Recorder) OnAgentLine(line []byte) (drain [][]byte, turn []byte) {
	var m struct {
		Method string          `json:"method"`
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Params struct {
			Update agentUpdate `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.Method == "session/update" {
		r.observeUpdateLocked(m.Params.Update)
		return nil, nil
	}
	isResponse := m.Method == "" && (len(m.Result) > 0 || m.Error != nil)
	matchesInflight := r.inflight == nil || (m.ID != nil && *m.ID == *r.inflight)
	if r.busy && isResponse && matchesInflight {
		return r.endTurnLocked()
	}
	return nil, nil
}

// endTurnLocked marks the current turn done and drains the next queued prompt (if any). Caller holds r.mu.
func (r *Recorder) endTurnLocked() (drain [][]byte, turn []byte) {
	r.busy = false
	r.inflight = nil
	if len(r.queue) > 0 {
		next := r.queue[0]
		r.queue = r.queue[1:]
		r.busy = true
		_, id, _ := promptText(next)
		r.inflight = id
		for i := range r.items {
			if r.items[i].Pending {
				r.items[i].Pending = false
				break
			}
		}
		return [][]byte{next}, r.turnFrameLocked()
	}
	return nil, r.turnFrameLocked()
}

// turnFrameLocked builds a spawn/turn notification, or nil if (state,queued) is unchanged since the
// last emit. Caller holds r.mu.
func (r *Recorder) turnFrameLocked() []byte {
	state := "idle"
	if r.busy {
		state = "busy"
	}
	cur := fmt.Sprintf("%s:%d", state, len(r.queue))
	if cur == r.lastTurn {
		return nil
	}
	r.lastTurn = cur
	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			State  string `json:"state"`
			Queued int    `json:"queued"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/turn"
	env.Params.State = state
	env.Params.Queued = len(r.queue)
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
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
// current transcript and turn-state, or nil if the transcript is empty (nothing to replay).
func (r *Recorder) HistoryFrame() []byte {
	r.mu.Lock()
	if len(r.items) == 0 {
		r.mu.Unlock()
		return nil
	}
	snap := make([]Item, len(r.items))
	copy(snap, r.items)
	state := "idle"
	if r.busy {
		state = "busy"
	}
	queued := len(r.queue)
	r.mu.Unlock()

	var env struct {
		Jsonrpc string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Items []Item `json:"items"`
			Turn  struct {
				State  string `json:"state"`
				Queued int    `json:"queued"`
			} `json:"turn"`
		} `json:"params"`
	}
	env.Jsonrpc = "2.0"
	env.Method = "spawn/history"
	env.Params.Items = snap
	env.Params.Turn.State = state
	env.Params.Turn.Queued = queued
	b, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return append(b, '\n')
}
