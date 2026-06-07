// Package acpmux is an in-container single-session ACP multiplexer. It is the
// SINGLE ACP client of an upstream agent (e.g. `goose acp` — spawned by the
// thin main, one shared session S, eager initialize + session/new at startup)
// AND a canonical-ACP SERVER that multiplexes N downstream ACP clients onto S.
//
// Downstream clients speak canonical ACP, so the node's existing pump
// (internal/node/pump.go — itself an ACP client) and a trivial acpdial shim
// both connect transparently. acpmux replays buffered conversation history to
// late joiners, fans every upstream session/update to all clients, serializes
// prompts across clients onto the one upstream session, and broadcasts each
// upstream permission request (first downstream response wins).
//
// The upstream/serialization/permission/replay-log mechanics are ported from
// internal/node/pump.go. The DIFFERENCE: pump's downstream wire is the node
// Frame protocol; acpmux's downstream wire is canonical ACP, and the replay log
// stores raw upstream session/update notification bytes (not Frames).
package acpmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"spawnery/internal/acp"
)

const defaultMaxLog = 2000 // cap the replay log; oldest trimmed (a lagging late-joiner loses the trimmed prefix)
const maxQueued = 50
const defaultPermTimeout = 2 * time.Minute

// logItem is one buffered outbound frame (raw JSON) with its monotonically
// assigned sequence number. A late-joining client replays every item with
// seq > its cursor whose target matches.
//
// target is the destination client id, or "" for a broadcast item. Broadcast
// items (session/update notifications) go to every client; targeted items (a
// turn-end prompt result) go only to the prompting client. Routing ALL outbound
// frames through this single ordered log — emitted by the per-client replay
// goroutine — guarantees a client sees its turn-end result strictly AFTER the
// turn's session/update chunks (the result is appended later, so it gets a
// higher seq), eliminating the cross-goroutine write race.
type logItem struct {
	seq    int64
	raw    []byte
	target string // "" = broadcast to all clients; otherwise a specific client id
}

// pendingPerm tracks one in-flight upstream permission request that has been
// broadcast to all downstream clients; the first client response wins.
type pendingPerm struct {
	upstreamID int             // the upstream agent request id to respond to
	options    json.RawMessage // raw options array, to pick allow/deny optionId
	title      string          // human-readable tool title (re-sent to late joiners)
	timer      *time.Timer
}

// queuedPrompt is one downstream prompt awaiting its turn on the single upstream
// session. The (clientID, clientReq) pair lets acpmux answer the originating
// client's session/prompt request EXACTLY when ITS turn ends.
type queuedPrompt struct {
	clientID  string
	clientReq *acp.RawID // the originating client's session/prompt id (string or int), echoed back verbatim at turn-end
	text      string
}

// dsClient is one connected downstream ACP client.
type dsClient struct {
	id         string
	conn       net.Conn      // the underlying connection (closed on Stop / detach)
	w          io.Writer     // the conn; writes serialized via wmu
	wmu        *sync.Mutex   // serialize writes to this conn (replay goroutine + read loop)
	cursor     int64         // last replay-log seq sent to this client
	notify     chan struct{} // buffered(1): "catch up on the replay log"
	done       chan struct{} // closed on conn close
	hasSession bool          // true once the client has session/new'd (gate replay)
}

// Mux owns the upstream agent connection (single ACP client) and the set of
// downstream ACP clients. All mutable fields are guarded by mu.
type Mux struct {
	stdin  io.Writer // upstream agent stdin (sole writer is writeLoop)
	stdout io.Reader // upstream agent stdout

	mu      sync.Mutex
	log     []logItem // log[i].seq == base+1+i (contiguous)
	base    int64     // seq of the last trimmed item (0 = nothing trimmed)
	seq     int64     // last assigned seq
	maxLog  int
	clients map[string]*dsClient
	stopped bool

	sessionID  string                   // S: the shared upstream session id
	initResult json.RawMessage          // cached upstream initialize result (returned to downstream)
	newModes   json.RawMessage          // cached upstream session/new `modes` block (advertised downstream, cat F)
	toAgent    chan []byte              // ndjson lines for writeLoop (sole upstream stdin writer)
	writerDone chan struct{}
	readerDone chan struct{}
	waiters    map[int]chan acp.Message // one-shot result waiters (our upstream requests: handshake)
	nextID     int                      // upstream JSON-RPC id allocator

	busy              bool
	queue             []queuedPrompt
	inflightPromptID  int        // upstream request id of the in-flight session/prompt (0 = none)
	inflightClient    string     // client whose prompt is in flight ("" = none/internal)
	inflightReq       *acp.RawID // that client's session/prompt request id (verbatim) to answer at turn-end
	inflightHasClient bool       // true if there is a downstream client to answer at turn-end

	pending     map[string]*pendingPerm // keyed by upstream request id (string)
	permTimeout time.Duration

	clientSeq int // downstream client id allocator
}

// New constructs a Mux over the upstream agent's stdin/stdout pipes.
func New(stdin io.Writer, stdout io.Reader) *Mux {
	return &Mux{
		stdin: stdin, stdout: stdout, maxLog: defaultMaxLog,
		clients:     map[string]*dsClient{},
		toAgent:     make(chan []byte, 64),
		writerDone:  make(chan struct{}),
		readerDone:  make(chan struct{}),
		waiters:     map[int]chan acp.Message{},
		pending:     map[string]*pendingPerm{},
		permTimeout: defaultPermTimeout,
	}
}

// SessionID returns the shared upstream session id (valid after Start).
func (m *Mux) SessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

// ---- upstream lifecycle -----------------------------------------------------

// Start launches the upstream read/write loops, performs the eager ACP
// handshake (initialize + session/new), caches the initialize result, and
// records the shared session id S. Returns once S is ready.
func (m *Mux) Start(ctx context.Context, readyTimeout time.Duration) error {
	go m.writeLoop()
	go m.readLoop()

	init, err := m.call(ctx, acp.Message{Method: "initialize", Params: json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`)}, readyTimeout)
	if err != nil {
		return fmt.Errorf("upstream not ready: %w", err)
	}
	m.mu.Lock()
	m.initResult = append([]byte(nil), init...)
	m.mu.Unlock()

	res, err := m.call(ctx, acp.Message{Method: "session/new", Params: json.RawMessage(`{"cwd":"/app","mcpServers":[]}`)}, readyTimeout)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var r struct {
		SessionID string          `json:"sessionId"`
		Modes     json.RawMessage `json:"modes"`
	}
	if uerr := json.Unmarshal(res, &r); uerr != nil || r.SessionID == "" {
		return fmt.Errorf("session/new: bad result %q (err %v)", string(res), uerr)
	}
	m.mu.Lock()
	m.sessionID = r.SessionID
	// Cache the upstream's advertised session modes (if any) so every downstream session/new response
	// re-advertises them (cat F). goose advertises none -> nil -> no modes downstream (graceful absence).
	if len(r.Modes) > 0 {
		m.newModes = append([]byte(nil), r.Modes...)
	}
	m.mu.Unlock()
	return nil
}

// Stop tears down the upstream loops and detaches all clients. Idempotent.
func (m *Mux) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.writerDone)
	if c, ok := m.stdout.(io.Closer); ok {
		_ = c.Close()
	}
	for _, c := range m.clients {
		close(c.done) // stop the per-client replay goroutine
		if c.conn != nil {
			// Also close the conn so the per-client READ loop (blocked in
			// ReadMessage) unblocks and its goroutine exits — otherwise Stop leaks a
			// read goroutine per client. net.Conn.Close is idempotent; the
			// handleClient defer's own Close is a harmless no-op after this.
			_ = c.conn.Close()
		}
	}
	m.clients = map[string]*dsClient{}
	for _, pp := range m.pending {
		pp.timer.Stop()
	}
	m.pending = map[string]*pendingPerm{}
	m.mu.Unlock()
}

func (m *Mux) writeLoop() {
	for {
		select {
		case line := <-m.toAgent:
			if _, err := m.stdin.Write(line); err != nil {
				return
			}
		case <-m.writerDone:
			return
		}
	}
}

func (m *Mux) sendLine(line []byte) {
	select {
	case m.toAgent <- line:
	case <-m.writerDone:
	}
}

// call sends an upstream ACP request (assigning an id) and waits for the
// matching result. The waiter is registered BEFORE sending so a fast reply
// can't be missed. Ported from pump.call.
func (m *Mux) call(ctx context.Context, msg acp.Message, timeout time.Duration) (json.RawMessage, error) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	ch := make(chan acp.Message, 1)
	m.waiters[id] = ch
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.waiters, id); m.mu.Unlock() }()
	msg.ID = acp.IntID(id)
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, msg)
	m.sendLine(buf.Bytes())
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
	case <-m.writerDone:
		return nil, fmt.Errorf("mux stopped")
	}
}

// sendPrompt fires a session/prompt at the upstream agent (no waiter) and
// records it as the in-flight turn, along with the originating client and its
// request id so acpmux can answer that client at turn-end. The id is set under
// the lock before enqueueing so a fast turn-end result can't race past it.
// Ported from pump.sendPrompt.
func (m *Mux) sendPrompt(sessionID, text, clientID string, clientReq *acp.RawID, hasClient bool) {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.inflightPromptID = id
	m.inflightClient = clientID
	m.inflightReq = clientReq
	m.inflightHasClient = hasClient
	m.mu.Unlock()
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: acp.IntID(id), Method: "session/prompt", Params: promptParams(sessionID, text)})
	m.sendLine(buf.Bytes())
}

// readLoop dispatches upstream messages: results to handshake waiters or, for
// the in-flight prompt id, to handleTurnEnd; notifications to fanout; upstream
// server-initiated permission requests to onPermissionRequest. Mirrors
// pump.readLoop, minus the Frame translation.
func (m *Mux) readLoop() {
	defer close(m.readerDone)
	rd := acp.NewReader(m.stdout)
	for {
		msg, err := rd.ReadMessage()
		if err != nil {
			return // upstream EOF/crash
		}
		// A response to one of OUR upstream requests (handshake / prompt). Our
		// upstream ids are always integers (we allocate them), so a string id
		// here is unexpected and AsInt's ok=false safely skips it.
		if idn, idok := msg.ID.AsInt(); idok && (msg.Result != nil || msg.Error != nil) {
			m.mu.Lock()
			ch, isWaiter := m.waiters[idn]
			inflight := m.inflightPromptID != 0 && idn == m.inflightPromptID
			m.mu.Unlock()
			if isWaiter {
				ch <- msg // handshake/our request result; not conversation
				continue
			}
			if inflight {
				// An upstream Error response to our in-flight prompt is a FAILED
				// turn — forward the failure downstream rather than masking it as a
				// clean end_turn.
				m.handleTurnEnd(msg.Error != nil)
			}
			continue
		}
		// A server-initiated request (has id + method): only permission requests.
		if msg.ID != nil && msg.Method == "session/request_permission" {
			m.onPermissionRequest(msg)
			continue
		}
		// A notification (no id).
		m.onUpstreamNotification(msg)
	}
}

func (m *Mux) onUpstreamNotification(msg acp.Message) {
	switch msg.Method {
	case "session/update":
		// Re-serialize so what we fan out is exactly the canonical notification.
		var buf bytes.Buffer
		if acp.WriteMessage(&buf, acp.Message{Method: "session/update", Params: msg.Params}) == nil {
			m.appendUpdate(buf.Bytes())
		}
	}
}

// handleTurnEnd clears busy on a turn-end, answers the in-flight client's
// session/prompt request (exact correlation), and drains one queued prompt (if
// any) as the next turn. Ported from pump.handleTurnEnd, plus the per-client
// reply that pump didn't need (pump's downstream is Frame, not ACP requests).
//
// The turn-end result is NOT written directly to the client conn. Instead it is
// APPENDED to the ordered replay log targeted at the prompting client, so it is
// emitted by that client's replay goroutine strictly AFTER the turn's buffered
// session/update chunks (which were appended earlier and thus have lower seqs).
// This is the only thing that guarantees the client doesn't see "turn complete"
// before the turn's last chunk. failed reports whether the upstream prompt
// returned an Error (turn failed) rather than a clean result.
func (m *Mux) handleTurnEnd(failed bool) {
	m.mu.Lock()
	endedClient := m.inflightClient
	endedReq := m.inflightReq
	endedHasClient := m.inflightHasClient
	m.busy = false
	m.inflightPromptID = 0
	m.inflightClient = ""
	m.inflightReq = nil
	m.inflightHasClient = false

	var next queuedPrompt
	var drained bool
	if len(m.queue) > 0 {
		next = m.queue[0]
		m.queue = m.queue[1:]
		drained = true
		m.busy = true
	}
	sid := m.sessionID
	_, clientExists := m.clients[endedClient]
	m.mu.Unlock()

	// Answer the just-ended prompt's originating client (turn complete or failed),
	// ordered after that turn's chunks via the per-client log.
	if endedHasClient && clientExists {
		result := json.RawMessage(`{"stopReason":"end_turn"}`)
		if failed {
			// Upstream prompt failed: don't mask it as a clean end_turn.
			result = json.RawMessage(`{"stopReason":"refusal"}`)
		}
		m.appendResultFor(endedClient, endedReq, result)
	}

	if drained {
		m.sendPrompt(sid, next.text, next.clientID, next.clientReq, true)
	}
}

// ---- downstream ACP server --------------------------------------------------

// Serve accepts downstream ACP client connections and handles each concurrently.
// It returns only on a fatal accept error. The upstream agent is NOT torn down
// when a client disconnects (other clients / late reconnects continue).
func (m *Mux) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go m.handleClient(conn)
	}
}

// handleClient registers a downstream client, runs its replay/stream goroutine,
// and reads canonical ACP messages from the conn until close. Every per-client
// goroutine exits on conn close — no leaks across connect/disconnect cycles.
func (m *Mux) handleClient(conn net.Conn) {
	m.mu.Lock()
	m.clientSeq++
	id := "c" + strconv.Itoa(m.clientSeq)
	c := &dsClient{
		id:     id,
		conn:   conn,
		w:      conn,
		wmu:    &sync.Mutex{},
		cursor: m.base, // start at the current trimmed-base; replay begins after session/new
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	m.clients[id] = c
	m.mu.Unlock()

	go m.clientReplayLoop(c)

	defer func() {
		m.mu.Lock()
		if cur := m.clients[id]; cur == c {
			delete(m.clients, id)
			close(c.done)
		}
		m.mu.Unlock()
		_ = conn.Close()
	}()

	rd := acp.NewReader(conn)
	for {
		msg, err := rd.ReadMessage()
		if err != nil {
			return // client disconnected
		}
		m.onClientMessage(c, msg)
	}
}

func (m *Mux) onClientMessage(c *dsClient, msg acp.Message) {
	// Responses from a client to acpmux's forwarded permission request carry an
	// id + a result, no method. The perm id is one we minted (an integer); a
	// non-integer id here is not a perm response, so AsInt's ok=false skips it.
	if idn, idok := msg.ID.AsInt(); idok && msg.Method == "" && (msg.Result != nil || msg.Error != nil) {
		m.onClientPermResponse(idn, msg.Result)
		return
	}
	switch msg.Method {
	case "initialize":
		m.mu.Lock()
		init := m.initResult
		m.mu.Unlock()
		if init == nil {
			init = json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{}}`)
		}
		if msg.ID != nil {
			m.respondToClient(c, msg.ID, init)
		}
	case "session/new", "session/load":
		m.mu.Lock()
		sid := m.sessionID
		modes := m.newModes
		c.hasSession = true
		m.mu.Unlock()
		if msg.ID != nil {
			out := map[string]any{"sessionId": sid}
			if len(modes) > 0 {
				out["modes"] = json.RawMessage(modes) // re-advertise upstream session modes (cat F)
			}
			res, _ := json.Marshal(out)
			m.respondToClient(c, msg.ID, res)
		}
		wake(c) // trigger replay of buffered history to this (possibly late-joining) client
	case "session/prompt":
		if msg.ID != nil {
			m.fromClientPrompt(c, msg.ID, promptText(msg.Params))
		}
	case "session/set_mode":
		// Forward a downstream mode switch to the shared upstream session (cat F). v1 shared-attach: any
		// client may set the mode (no arbitration). The upstream current_mode_update notification fans out
		// to every client via onUpstreamNotification, so all selectors follow the switch.
		m.forwardSetMode(msg.Params)
	case "session/cancel":
		// best-effort: v1 no-op (single-session, serialized; cancel not modeled).
	}
}

// fromClientPrompt runs a downstream prompt through the shared busy/queue
// serializer onto the single upstream session. One in-flight session/prompt at
// a time; the rest queue (FIFO) and drain on turn-end. Ported from
// pump.fromClient's "prompt" case, with per-client request-id tracking.
func (m *Mux) fromClientPrompt(c *dsClient, clientReq *acp.RawID, text string) {
	m.mu.Lock()
	sid := m.sessionID
	if !m.busy {
		m.busy = true
		m.mu.Unlock()
		m.sendPrompt(sid, text, c.id, clientReq, true)
		return
	}
	if len(m.queue) < maxQueued {
		m.queue = append(m.queue, queuedPrompt{clientID: c.id, clientReq: clientReq, text: text})
		m.mu.Unlock()
		return
	}
	m.mu.Unlock() // over cap -> drop
	// Route the refusal through the per-client log too, so the replay goroutine
	// remains the SOLE writer of session/prompt result frames to a client conn.
	m.appendResultFor(c.id, clientReq, json.RawMessage(`{"stopReason":"refusal"}`))
}

// forwardSetMode relays a downstream session/set_mode to the shared upstream session (cat F). The modeId
// from the client's params is re-sent against the shared session id with a freshly minted upstream id.
// Fire-and-forget: the upstream result is ignored (a non-waiter, non-inflight id readLoop simply skips);
// the switch is confirmed to all clients by the upstream current_mode_update that fans out afterwards.
func (m *Mux) forwardSetMode(params json.RawMessage) {
	var p struct {
		ModeID string `json:"modeId"`
	}
	if json.Unmarshal(params, &p) != nil || p.ModeID == "" {
		return
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	sid := m.sessionID
	m.mu.Unlock()
	out, _ := json.Marshal(map[string]any{"sessionId": sid, "modeId": p.ModeID})
	var buf bytes.Buffer
	if acp.WriteMessage(&buf, acp.Message{ID: acp.IntID(id), Method: "session/set_mode", Params: out}) != nil {
		return
	}
	m.sendLine(buf.Bytes())
}

// ---- replay log + fanout ----------------------------------------------------

// appendUpdate appends a broadcast session/update notification to the replay
// log (target "" = every client). Ported from pump.appendFrames.
func (m *Mux) appendUpdate(raw []byte) {
	m.appendItem("", raw)
}

// appendResultFor appends a JSON-RPC result for the given downstream request id
// to the replay log, TARGETED at one client. Routed through the ordered log so
// it is emitted after any earlier (lower-seq) items for that client — in
// particular the session/update chunks of the turn it completes.
func (m *Mux) appendResultFor(clientID string, reqID *acp.RawID, result json.RawMessage) {
	var buf bytes.Buffer
	if acp.WriteMessage(&buf, acp.Message{ID: reqID, Result: result}) != nil {
		return
	}
	m.appendItem(clientID, buf.Bytes())
}

// appendItem assigns a seq, appends the raw frame to the replay log (trimming
// the oldest past maxLog), and wakes all clients to catch up. target "" means
// broadcast; otherwise the item is delivered only to the named client (other
// clients skip it but still advance their cursor past it).
func (m *Mux) appendItem(target string, raw []byte) {
	m.mu.Lock()
	m.seq++
	m.log = append(m.log, logItem{seq: m.seq, raw: append([]byte(nil), raw...), target: target})
	if over := len(m.log) - m.maxLog; over > 0 {
		m.base += int64(over)
		m.log = append(m.log[:0], m.log[over:]...)
	}
	for _, c := range m.clients {
		wake(c)
	}
	m.mu.Unlock()
}

func wake(c *dsClient) {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// clientReplayLoop ships replay-log items > c.cursor whenever woken, until done.
// Only clients that have established a session receive replay (so a client that
// hasn't session/new'd yet doesn't get notifications for a session it doesn't
// know about). Ported from pump.clientLoop, minus the reset frame (ACP has no
// such frame; a lagging late-joiner simply loses the trimmed prefix).
func (m *Mux) clientReplayLoop(c *dsClient) {
	for {
		select {
		case <-c.done:
			return
		case <-c.notify:
		}
		for {
			m.mu.Lock()
			if !c.hasSession {
				m.mu.Unlock()
				break
			}
			if c.cursor < m.base {
				c.cursor = m.base // missed trimmed items; skip them
			}
			var batch [][]byte
			if c.cursor < m.seq {
				from := c.cursor - m.base // index of first unseen item
				for _, it := range m.log[from:] {
					// Broadcast items go to everyone; targeted items only to their
					// target. Non-matching items are skipped but the cursor still
					// advances past them (set to m.seq below), so they are never
					// re-examined — preserving strict per-client total ordering.
					if it.target == "" || it.target == c.id {
						batch = append(batch, it.raw)
					}
				}
				c.cursor = m.seq
			}
			m.mu.Unlock()
			if len(batch) == 0 {
				break
			}
			for _, raw := range batch {
				if m.writeRaw(c, raw) != nil {
					return
				}
			}
		}
	}
}

// ---- permissions (broadcast + first-wins) -----------------------------------

// onPermissionRequest records an upstream permission request, broadcasts a
// canonical session/request_permission to every connected client (using the
// upstream request id as the downstream id so client responses correlate), and
// arms a timeout that auto-denies. Ported from pump.onPermissionRequest.
func (m *Mux) onPermissionRequest(msg acp.Message) {
	upID, ok := msg.ID.AsInt()
	if !ok {
		return // upstream (goose) permission ids are integers; ignore anything else
	}
	reqID := strconv.Itoa(upID)
	var pr struct {
		Options  json.RawMessage `json:"options"`
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
	}
	_ = json.Unmarshal(msg.Params, &pr)
	title := pr.ToolCall.Title
	if title == "" {
		title = "permission requested"
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	pp := &pendingPerm{upstreamID: upID, options: pr.Options, title: title}
	// Unanswered -> auto-deny: "" selects a reject-ish option from the upstream set.
	pp.timer = time.AfterFunc(m.permTimeout, func() { m.resolvePermission(reqID, "") })
	m.pending[reqID] = pp
	clients := make([]*dsClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.Unlock()

	// Forward the upstream request verbatim downstream, preserving the same id so
	// each client's response carries it (the first answer wins in resolve).
	//
	// NOTE (v1 cosmetic limitation): permission requests are broadcast OUT-OF-BAND
	// here, NOT through the ordered per-client replay log used for session/update
	// chunks and turn-end results. They are deliberately kept out of the log
	// because perms must NOT be replayed to late joiners as stale prompts (only
	// still-pending perms are re-sent on attach, by the pump). The cost is that a
	// session/request_permission may, in rare interleavings, reach a client
	// slightly before the session/update (tool_call) that motivated it — a purely
	// cosmetic display-ordering quirk, not a correctness issue (the perm id and
	// first-wins resolution are unaffected). Promoting perms into the ordered log
	// without re-introducing stale-perm replay is deferred.
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: msg.ID, Method: "session/request_permission", Params: msg.Params})
	line := buf.Bytes()
	for _, c := range clients {
		_ = m.writeRaw(c, line)
	}
}

// onClientPermResponse routes a downstream client's permission answer to
// resolvePermission, forwarding the EXACT optionId the client selected upstream
// (no binary allow/deny collapse — a client's allow_always stays allow_always).
// The downstream id equals the upstream id (see onPermissionRequest). First
// answer wins; a cancelled/unknown outcome maps to an auto-deny ("").
func (m *Mux) onClientPermResponse(id int, result json.RawMessage) {
	reqID := strconv.Itoa(id)
	m.resolvePermission(reqID, selectedOptionID(result))
}

// resolvePermission answers a pending upstream permission (first answer wins;
// later/duplicate are no-ops) by forwarding the chosen optionId upstream. Called
// by a client response (with the selected optionId) and by the auto-deny timer
// (with "" — which selects a reject-ish upstream option).
func (m *Mux) resolvePermission(reqID string, optID string) {
	m.mu.Lock()
	pp := m.pending[reqID]
	if pp == nil {
		m.mu.Unlock()
		return // already resolved
	}
	delete(m.pending, reqID)
	pp.timer.Stop()
	upstreamID := pp.upstreamID
	if optID == "" {
		optID = rejectOptionID(pp.options) // auto-deny / cancelled: pick a reject-ish option
	}
	m.mu.Unlock()

	resp, _ := json.Marshal(map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optID}})
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: acp.IntID(upstreamID), Result: resp})
	m.sendLine(buf.Bytes())
}

// ---- small helpers ----------------------------------------------------------

// respondToClient writes a JSON-RPC result echoing the client's request id
// VERBATIM (string or integer), so clients like nori that use string UUID ids
// can correlate the response.
func (m *Mux) respondToClient(c *dsClient, id *acp.RawID, result json.RawMessage) {
	var buf bytes.Buffer
	_ = acp.WriteMessage(&buf, acp.Message{ID: id, Result: result})
	_ = m.writeRaw(c, buf.Bytes())
}

// writeRaw writes one already-encoded ndjson line to a client, serializing
// writes per-connection (the replay goroutine and the read loop both write).
func (m *Mux) writeRaw(c *dsClient, line []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.w.Write(line)
	return err
}

func promptParams(sessionID, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	})
	return b
}

// promptText extracts the concatenated text content blocks from a downstream
// session/prompt params object.
func promptText(params json.RawMessage) string {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if json.Unmarshal(params, &p) != nil {
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

// selectedOptionID extracts the optionId a downstream client selected. ACP responses carry
// {"outcome":{"outcome":"selected","optionId":...}} or {"outcome":{"outcome":"cancelled"}}; a
// cancelled/unknown outcome yields "" (the caller treats "" as an auto-deny).
func selectedOptionID(result json.RawMessage) string {
	var r struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if json.Unmarshal(result, &r) != nil || r.Outcome.Outcome != "selected" {
		return ""
	}
	return r.Outcome.OptionID
}

// rejectOptionID picks a reject-ish optionId from the upstream options for the auto-deny / cancelled
// path, falling back to the first option (then "" if there are none).
func rejectOptionID(options json.RawMessage) string {
	var opts []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	}
	_ = json.Unmarshal(options, &opts)
	for _, o := range opts {
		if strings.Contains(o.Kind, "reject") || strings.Contains(o.Kind, "deny") {
			return o.OptionID
		}
	}
	if len(opts) > 0 {
		return opts[0].OptionID
	}
	return ""
}
