package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"spawnery/internal/acp"
)

const defaultMaxLog = 2000 // cap the per-spawn frame log; oldest trimmed (a lagging client gets reset)
const maxQueued = 50

type client struct {
	cursor int64 // last seq this client has been sent
	send   frameSender
	notify chan struct{} // buffered(1): "catch up"
	done   chan struct{}
}

// frameSender delivers one encoded frame line to a client; returns an error if the client is gone.
type frameSender func(line []byte) error

// Pump is the long-lived per-spawn relay: it owns the goose stdio, an append-only frame log, and a
// set of client subscribers. Built across Tasks 2-4. All mutable fields behind mu.
type Pump struct {
	stdin  io.Writer
	stdout io.Reader

	mu      sync.Mutex
	log     []Frame // log[i].Seq == base+1+i (contiguous)
	base    int64   // seq of the last trimmed frame (0 = nothing trimmed)
	seq     int64   // last assigned seq
	maxLog  int
	clients map[string]*client
	stopped bool // set by stop() in Task 4 (agent teardown); unused in the fan-out core

	sessionID        string
	toAgent          chan []byte          // ndjson lines for the writer (sole stdin writer)
	writerDone       chan struct{}
	waiters          map[int]chan acp.Message // one-shot result waiters (handshake/our requests)
	nextID           int
	busy             bool
	queue            []string             // queued prompt texts, FIFO
	inflightPromptID int                  // goose request id of the in-flight session/prompt (0 = none)
}

func newPump(stdin io.Writer, stdout io.Reader) *Pump {
	return &Pump{
		stdin: stdin, stdout: stdout, maxLog: defaultMaxLog,
		clients:    map[string]*client{},
		toAgent:    make(chan []byte, 64),
		writerDone: make(chan struct{}),
		waiters:    map[int]chan acp.Message{},
	}
}

// appendFrames assigns seqs, appends to the log (trimming the oldest past maxLog), and wakes clients.
func (p *Pump) appendFrames(fs []Frame) {
	if len(fs) == 0 {
		return
	}
	p.mu.Lock()
	for i := range fs {
		p.seq++
		fs[i].Seq = p.seq
		p.log = append(p.log, fs[i])
	}
	if over := len(p.log) - p.maxLog; over > 0 {
		p.base += int64(over)
		p.log = append(p.log[:0], p.log[over:]...)
	}
	for _, c := range p.clients {
		wake(c)
	}
	p.mu.Unlock()
}

func wake(c *client) {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (p *Pump) attachClient(clientID string, cursor int64, send frameSender) {
	p.mu.Lock()
	if old := p.clients[clientID]; old != nil {
		close(old.done) // replace same id (defensive; normally ids are unique per connection)
	}
	c := &client{cursor: cursor, send: send, notify: make(chan struct{}, 1), done: make(chan struct{})}
	p.clients[clientID] = c
	p.mu.Unlock()
	wake(c) // initial catch-up
	go p.clientLoop(c)
}

func (p *Pump) detachClient(clientID string) {
	p.mu.Lock()
	if c := p.clients[clientID]; c != nil {
		close(c.done)
		delete(p.clients, clientID)
	}
	p.mu.Unlock()
}

// clientLoop ships log frames > c.cursor whenever woken, until done.
func (p *Pump) clientLoop(c *client) {
	for {
		select {
		case <-c.done:
			return
		case <-c.notify:
		}
		for {
			p.mu.Lock()
			// If the client's cursor is outside the retained window [base, seq] -- it missed trimmed
			// frames (cursor < base) OR it is ahead of us (cursor > seq, e.g. the pump restarted and
			// seq reset to 0 while the client kept an old cursor) -- reset it to base and replay.
			var reset *Frame
			if c.cursor < p.base || c.cursor > p.seq {
				r := Frame{Kind: "reset", FromSeq: p.base}
				reset = &r
				c.cursor = p.base
			}
			var batch []Frame
			if c.cursor < p.seq {
				from := c.cursor - p.base // index of first unseen frame
				batch = append(batch, p.log[from:]...)
				c.cursor = p.seq
			}
			p.mu.Unlock()
			if reset == nil && len(batch) == 0 {
				break
			}
			if reset != nil {
				if c.send(encodeFrame(*reset)) != nil {
					return
				}
			}
			for _, f := range batch {
				if c.send(encodeFrame(f)) != nil {
					return
				}
			}
		}
	}
}

func (p *Pump) start(ctx context.Context, readyTimeout time.Duration) error {
	go p.writeLoop()
	go p.readLoop()
	if _, err := p.call(acp.Message{Method: "initialize", Params: json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`)}, readyTimeout); err != nil {
		return fmt.Errorf("agent not ready: %w", err)
	}
	res, err := p.call(acp.Message{Method: "session/new", Params: json.RawMessage(`{"cwd":"/app","mcpServers":[]}`)}, readyTimeout)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &r)
	p.mu.Lock()
	p.sessionID = r.SessionID
	p.mu.Unlock()
	return nil
}

func (p *Pump) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.writerDone)
	for _, c := range p.clients {
		close(c.done)
	}
	p.clients = map[string]*client{}
	p.mu.Unlock()
}

func (p *Pump) writeLoop() {
	for {
		select {
		case line := <-p.toAgent:
			if _, err := p.stdin.Write(line); err != nil {
				return
			}
		case <-p.writerDone:
			return
		}
	}
}

func (p *Pump) sendLine(line []byte) {
	select {
	case p.toAgent <- line:
	case <-p.writerDone:
	}
}

// call sends an ACP request (assigning a JSON-RPC id) and waits for the matching result. The waiter is
// registered BEFORE sending so a fast agent reply can't be missed.
func (p *Pump) call(m acp.Message, timeout time.Duration) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan acp.Message, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.waiters, id); p.mu.Unlock() }()
	idv := id
	m.ID = &idv
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, m)
	p.sendLine(buf.Bytes())
	select {
	case rm := <-ch:
		if rm.Error != nil {
			return nil, fmt.Errorf("rpc %d: %s", rm.Error.Code, rm.Error.Message)
		}
		return rm.Result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout after %s", timeout)
	case <-p.writerDone:
		return nil, fmt.Errorf("pump stopped")
	}
}

// sendPrompt fires a session/prompt at the agent (no waiter) and records it as the in-flight turn.
// The id is set under the lock before enqueueing so a fast turn-end result can't race past it.
func (p *Pump) sendPrompt(sessionID, text string) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.inflightPromptID = id
	p.mu.Unlock()
	idv := id
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: &idv, Method: "session/prompt", Params: promptParams(sessionID, text)})
	p.sendLine(buf.Bytes())
}

func (p *Pump) readLoop() {
	rd := acp.NewReader(p.stdout)
	for {
		m, err := rd.ReadMessage()
		if err != nil {
			return // agent EOF/crash
		}
		if m.ID != nil && (m.Result != nil || m.Error != nil) {
			p.mu.Lock()
			ch, isWaiter := p.waiters[*m.ID]
			inflight := p.inflightPromptID != 0 && *m.ID == p.inflightPromptID
			p.mu.Unlock()
			if isWaiter {
				ch <- m // handshake/our request result; not conversation
				continue
			}
			if inflight {
				p.handleTurnEnd() // session/prompt result = turn-end
			}
			continue
		}
		p.onAgentNotification(m)
	}
}

func (p *Pump) onAgentNotification(m acp.Message) {
	switch m.Method {
	case "session/update":
		if f, ok := updateToFrame(m.Params); ok {
			p.appendFrames([]Frame{f})
		}
		// session/request_permission is handled in Task 4.
	}
}

// updateToFrame translates a goose session/update params object into one conversation Frame.
func updateToFrame(params json.RawMessage) (Frame, bool) {
	var u struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Status     string `json:"status"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &u) != nil {
		return Frame{}, false
	}
	switch u.Update.SessionUpdate {
	case "agent_message_chunk":
		return Frame{Kind: "agent", Text: u.Update.Content.Text}, true
	case "agent_thought_chunk":
		return Frame{Kind: "thought", Text: u.Update.Content.Text}, true
	case "tool_call":
		return Frame{Kind: "tool", ToolID: u.Update.ToolCallID, Title: u.Update.Title, Status: u.Update.Status}, true
	case "tool_call_update":
		return Frame{Kind: "tool", ToolID: u.Update.ToolCallID, Status: u.Update.Status}, true
	}
	return Frame{}, false
}

// handleTurnEnd clears busy on a turn-end and drains one queued prompt (if any) as the next turn.
func (p *Pump) handleTurnEnd() {
	p.mu.Lock()
	p.busy = false
	p.inflightPromptID = 0
	var drainText string
	var drained bool
	if len(p.queue) > 0 {
		drainText = p.queue[0]
		p.queue = p.queue[1:]
		drained = true
		p.busy = true
	}
	queued := len(p.queue)
	sid := p.sessionID
	p.mu.Unlock()

	if drained {
		// the drained prompt's user frame was already logged when it was queued; just send it + busy.
		p.appendFrames([]Frame{{Kind: "turn", State: "busy", Queued: queued}})
		p.sendPrompt(sid, drainText)
		return
	}
	p.appendFrames([]Frame{{Kind: "turn", State: "idle", Queued: 0}})
}

// fromClient handles a client->pump frame.
func (p *Pump) fromClient(clientID string, line []byte) {
	f, err := decodeFrame(line)
	if err != nil {
		return
	}
	switch f.Kind {
	case "prompt":
		p.mu.Lock()
		sid := p.sessionID
		if !p.busy {
			p.busy = true
			p.mu.Unlock()
			p.appendFrames([]Frame{{Kind: "user", Text: f.Text}, {Kind: "turn", State: "busy", Queued: 0}})
			p.sendPrompt(sid, f.Text)
			return
		}
		if len(p.queue) < maxQueued {
			p.queue = append(p.queue, f.Text)
			queued := len(p.queue)
			p.mu.Unlock()
			p.appendFrames([]Frame{{Kind: "user", Text: f.Text}, {Kind: "turn", State: "busy", Queued: queued}})
			return
		}
		p.mu.Unlock() // over cap -> drop (the web also gates on MAX_QUEUED)
		// perm_response is handled in Task 4.
	}
}

func promptParams(sessionID, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	})
	return b
}
