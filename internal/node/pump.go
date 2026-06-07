package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"spawnery/internal/acp"
)

const defaultMaxLog = 2000 // cap the per-spawn frame log; oldest trimmed (a lagging client gets reset)
const maxQueued = 50
const defaultPermTimeout = 2 * time.Minute

type pendingPerm struct {
	agentID int             // the agent request id to respond to
	options json.RawMessage // raw options array, to pick allow/deny optionId
	title   string          // human-readable tool title from agent (for the perm_request frame + re-send)
	timer   *time.Timer
}

type client struct {
	cursor int64 // last seq this client has been sent
	send   frameSender
	notify chan struct{} // buffered(1): "catch up"
	done   chan struct{}
}

// frameSender delivers one encoded frame line to a client; returns an error if the client is gone.
// It MUST be safe for concurrent use: the pump calls it both from a client's clientLoop goroutine and
// directly (the perm_request broadcast / attach re-send). The integration's sender is a non-blocking
// channel write; tests use a mutex-guarded capture.
type frameSender func(line []byte) error

// Pump is the long-lived per-spawn relay: it owns the agent stdio, an append-only frame log, and a
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
	stopped bool // guards stop() against double-teardown

	sessionID        string
	toAgent          chan []byte               // ndjson lines for the writer (sole stdin writer)
	writerDone       chan struct{}
	readerDone       chan struct{}
	waiters          map[int]chan acp.Message // one-shot result waiters (handshake/our requests)
	nextID           int
	busy             bool
	queue            []string // queued prompt texts, FIFO
	inflightPromptID int      // agent request id of the in-flight session/prompt (0 = none)

	pending     map[string]*pendingPerm
	permTimeout time.Duration

	lastActivity time.Time // last relay frame in EITHER direction; the idle reaper's clock

	closeFn func() error // agent-attach teardown (set by the integration); preferred over the stdout cast
	exitFn  func()       // called when readLoop exits on AGENT DEATH (not on intentional stop)
}

func newPump(stdin io.Writer, stdout io.Reader) *Pump {
	return &Pump{
		stdin: stdin, stdout: stdout, maxLog: defaultMaxLog,
		clients:      map[string]*client{},
		toAgent:      make(chan []byte, 64),
		writerDone:   make(chan struct{}),
		readerDone:   make(chan struct{}),
		waiters:      map[int]chan acp.Message{},
		pending:      map[string]*pendingPerm{},
		permTimeout:  defaultPermTimeout,
		lastActivity: time.Now(), // start the idle clock at creation, not the zero time
	}
}

// lastActive reports the time of the most recent relay frame in either direction.
func (p *Pump) lastActive() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastActivity
}

// attached reports whether any client is currently subscribed (the two-stage idle reaper gives
// attached spawns a longer budget than detached ones).
func (p *Pump) attached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients) > 0
}

// markActive refreshes the idle clock. Callers must NOT already hold mu.
func (p *Pump) markActive() {
	p.mu.Lock()
	p.lastActivity = time.Now()
	p.mu.Unlock()
}

// appendFrames assigns seqs, appends to the log (trimming the oldest past maxLog), and wakes clients.
func (p *Pump) appendFrames(fs []Frame) {
	if len(fs) == 0 {
		return
	}
	p.mu.Lock()
	p.lastActivity = time.Now() // agent->client relay = activity
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
	// Snapshot still-pending perm requests to re-send to this client. The snapshot may include a perm
	// resolved concurrently (we send after unlock) -> the late client briefly shows an already-answered
	// prompt; its later response no-ops in resolvePermission. Acceptable for an interactive prompt.
	var perms [][]byte
	for reqID, pp := range p.pending {
		perms = append(perms, encodeFrame(Frame{Kind: "perm_request", ReqID: reqID, Title: pp.title}))
	}
	p.mu.Unlock()
	for _, line := range perms {
		_ = send(line) // re-send still-pending perm requests to the newly attached client
	}
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
	if _, err := p.call(ctx, acp.Message{Method: "initialize", Params: json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`)}, readyTimeout); err != nil {
		return fmt.Errorf("agent not ready: %w", err)
	}
	res, err := p.call(ctx, acp.Message{Method: "session/new", Params: json.RawMessage(`{"cwd":"/app","mcpServers":[]}`)}, readyTimeout)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if uerr := json.Unmarshal(res, &r); uerr != nil || r.SessionID == "" {
		return fmt.Errorf("session/new: bad result %q (err %v)", string(res), uerr)
	}
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
	if p.closeFn != nil {
		_ = p.closeFn()
	} else if c, ok := p.stdout.(io.Closer); ok {
		_ = c.Close()
	}
	for _, c := range p.clients {
		close(c.done)
	}
	p.clients = map[string]*client{}
	for _, pp := range p.pending {
		pp.timer.Stop()
	}
	p.pending = map[string]*pendingPerm{}
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
func (p *Pump) call(ctx context.Context, m acp.Message, timeout time.Duration) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan acp.Message, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.waiters, id); p.mu.Unlock() }()
	m.ID = acp.IntID(id)
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, m)
	p.sendLine(buf.Bytes())
	select {
	case rm := <-ch:
		if rm.Error != nil {
			return nil, fmt.Errorf("rpc %d: %s", rm.Error.Code, rm.Error.Message)
		}
		return rm.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: acp.IntID(id), Method: "session/prompt", Params: promptParams(sessionID, text)})
	p.sendLine(buf.Bytes())
}

func (p *Pump) readLoop() {
	defer func() {
		close(p.readerDone)
		p.mu.Lock()
		stopped := p.stopped
		fn := p.exitFn
		p.mu.Unlock()
		if !stopped && fn != nil {
			fn()
		}
	}()
	rd := acp.NewReader(p.stdout)
	for {
		m, err := rd.ReadMessage()
		if err != nil {
			return // agent EOF/crash
		}
		if idn, idok := m.ID.AsInt(); idok && (m.Result != nil || m.Error != nil) {
			p.mu.Lock()
			ch, isWaiter := p.waiters[idn]
			inflight := p.inflightPromptID != 0 && idn == p.inflightPromptID
			p.mu.Unlock()
			if isWaiter {
				ch <- m // handshake/our request result; not conversation
				continue
			}
			if inflight {
				p.handleTurnEnd(m.Result) // session/prompt result = turn-end (carries stopReason + error)
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
	case "session/request_permission":
		p.onPermissionRequest(m)
	}
}

// updateToFrame translates a agent session/update params object into one conversation Frame.
// Content is decoded raw because text chunks carry a single content OBJECT while tool calls carry a
// content ARRAY of ToolCallContent blocks — the two shapes share the `content` JSON key.
func updateToFrame(params json.RawMessage) (Frame, bool) {
	var u struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			RawOutput     json.RawMessage `json:"rawOutput"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &u) != nil {
		return Frame{}, false
	}
	up := u.Update
	switch up.SessionUpdate {
	case "user_message_chunk":
		// A user message from ANOTHER client (e.g. the in-container TUI), echoed so the web shows it.
		// The node's own web-submitted prompts are echoed locally in fromClient, not here.
		return Frame{Kind: "user", Text: textContent(up.Content)}, true
	case "agent_message_chunk":
		return Frame{Kind: "agent", Text: textContent(up.Content)}, true
	case "agent_thought_chunk":
		return Frame{Kind: "thought", Text: textContent(up.Content)}, true
	case "tool_call":
		f := Frame{Kind: "tool", ToolID: up.ToolCallID, Title: up.Title, Status: up.Status}
		f.Tool = toolPayload(up.Content, up.RawInput, up.RawOutput)
		return f, true
	case "tool_call_update":
		f := Frame{Kind: "tool", ToolID: up.ToolCallID, Status: up.Status}
		f.Tool = toolPayload(up.Content, up.RawInput, up.RawOutput)
		return f, true
	}
	return Frame{}, false
}

// textContent extracts the `text` field of a single content OBJECT (text/thought/user chunks).
// Returns "" if content is absent or an array (a tool-call content shape) rather than an object.
func textContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var c struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &c) // array/other shapes fail silently -> ""
	return c.Text
}

// toolPayload builds a *ToolPayload from an ACP tool-call's content ARRAY (ToolCallContent blocks) and
// raw I/O. Returns nil when there is nothing to carry, so existing title+status-only frames stay lean.
func toolPayload(content, rawIn, rawOut json.RawMessage) *ToolPayload {
	var blocks []ContentBlock
	var diff *Diff
	if len(content) > 0 {
		var arr []struct {
			Type    string `json:"type"` // "content" | "diff"
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Path    string `json:"path"` // type=="diff"
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		}
		if json.Unmarshal(content, &arr) == nil { // object shape (text chunk) fails -> no blocks
			for _, e := range arr {
				switch e.Type {
				case "diff":
					if e.Path != "" || e.OldText != "" || e.NewText != "" {
						diff = &Diff{Path: e.Path, OldText: e.OldText, NewText: e.NewText}
					}
				default:
					if e.Content.Type == "text" && e.Content.Text != "" {
						blocks = append(blocks, ContentBlock{Type: "text", Text: e.Content.Text})
					}
				}
			}
		}
	}
	if len(blocks) == 0 && diff == nil && len(rawIn) == 0 && len(rawOut) == 0 {
		return nil
	}
	tp := &ToolPayload{Content: blocks}
	if diff != nil {
		tp.Diff = diff
	}
	if len(rawIn) > 0 {
		tp.RawInput = rawIn
	}
	if len(rawOut) > 0 {
		tp.RawOutput = rawOut
	}
	return tp
}

// handleTurnEnd clears busy on a turn-end and drains one queued prompt (if any) as the next turn. The
// session/prompt result carries the ACP StopReason and (for opencode) a structured error; these ride
// the emitted idle turn Frame as Reason/Error so the client can show an honest turn-ending indicator.
func (p *Pump) handleTurnEnd(result json.RawMessage) {
	reason, errInfo := parseStopResult(result)
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
	// Reason/Error are omitempty: a normal end_turn with no error serializes byte-identically to before.
	p.appendFrames([]Frame{{Kind: "turn", State: "idle", Queued: 0, Reason: turnReason(reason), Error: errInfo}})
}

// parseStopResult extracts the ACP StopReason and an optional structured error from a session/prompt
// result. Best-effort: a missing/garbled result (e.g. a goose agent that omits stopReason) yields "".
func parseStopResult(result json.RawMessage) (string, *ErrorInfo) {
	if len(result) == 0 {
		return "", nil
	}
	var r struct {
		StopReason string `json:"stopReason"`
		Error      *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(result, &r) != nil {
		return "", nil
	}
	var ei *ErrorInfo
	if r.Error != nil && (r.Error.Code != 0 || r.Error.Message != "") {
		ei = &ErrorInfo{Code: r.Error.Code, Message: r.Error.Message}
	}
	return r.StopReason, ei
}

// turnReason collapses a normal end_turn (or an absent reason) to "" so the idle turn Frame stays
// byte-stable; any non-normal reason (max_tokens|max_turn_requests|refusal|cancelled) is carried through.
func turnReason(stop string) string {
	if stop == "" || stop == "end_turn" {
		return ""
	}
	return stop
}

// fromClient handles a client->pump frame.
func (p *Pump) fromClient(clientID string, line []byte) {
	f, err := decodeFrame(line)
	if err != nil {
		return
	}
	p.markActive() // client->pump relay = activity
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
	case "perm_response":
		p.resolvePermission(f.ReqID, f.Allow)
	}
}

func promptParams(sessionID, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	})
	return b
}

// onPermissionRequest records a agent permission request, broadcasts a transient perm_request to all
// attached clients (NOT logged), and arms a timeout that auto-denies.
func (p *Pump) onPermissionRequest(m acp.Message) {
	agentID, ok := m.ID.AsInt()
	if !ok {
		return // agent permission request ids are integers
	}
	reqID := strconv.Itoa(agentID)
	var pr struct {
		Options  json.RawMessage `json:"options"`
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
	}
	_ = json.Unmarshal(m.Params, &pr)
	title := pr.ToolCall.Title
	if title == "" {
		title = "permission requested" // agent omitted a tool title; fall back to a generic label
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	pp := &pendingPerm{agentID: agentID, options: pr.Options, title: title}
	pp.timer = time.AfterFunc(p.permTimeout, func() { p.resolvePermission(reqID, false) })
	p.pending[reqID] = pp
	clients := make([]frameSender, 0, len(p.clients))
	for _, c := range p.clients {
		clients = append(clients, c.send)
	}
	p.mu.Unlock()
	line := encodeFrame(Frame{Kind: "perm_request", ReqID: reqID, Title: title})
	for _, send := range clients {
		_ = send(line)
	}
}

// resolvePermission answers a pending permission (first answer wins; later/duplicate are no-ops) by
// forwarding the chosen option to agent. Called by perm_response and by the auto-deny timer.
func (p *Pump) resolvePermission(reqID string, allow bool) {
	p.mu.Lock()
	pp := p.pending[reqID]
	if pp == nil {
		p.mu.Unlock()
		return // already resolved
	}
	delete(p.pending, reqID)
	pp.timer.Stop()
	agentID := pp.agentID
	optID := pickPermOption(pp.options, allow)
	p.mu.Unlock()
	resp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optID}})
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: acp.IntID(agentID), Result: resp})
	p.sendLine(buf.Bytes())
}

// pickPermOption chooses an allow-ish (or reject-ish) optionId from the agent options, falling back to
// the first option. Mirrors web/src/acp/client.ts handlePermission.
func pickPermOption(options json.RawMessage, allow bool) string {
	var opts []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	}
	_ = json.Unmarshal(options, &opts)
	want := []string{"reject", "deny"}
	if allow {
		want = []string{"allow"}
	}
	for _, o := range opts {
		for _, w := range want {
			if strings.Contains(o.Kind, w) {
				return o.OptionID
			}
		}
	}
	if len(opts) > 0 {
		return opts[0].OptionID
	}
	return ""
}
