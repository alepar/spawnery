package ocadapter

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/opencode"
)

// sessionFile is where the adapter records the spawn's opencode session id so the in-container TUI
// launcher (spawn-tui) can pin `opencode attach -s <id>` to the same shared session. Overridable for
// tests via SPAWNERY_SESSION_FILE.
func sessionFilePath() string {
	if p := os.Getenv("SPAWNERY_SESSION_FILE"); p != "" {
		return p
	}
	return "/tmp/spawnery-opencode-session"
}

// Adapter presents a canonical-ACP agent to the node, backed by an opencode
// server. One Adapter serves one node connection; the spawn maps to a single
// opencode session (discover-or-create, resume-safe).
type Adapter struct {
	oc    *opencode.Client
	dir   string // the spawn working dir; scopes session discovery
	model string // "providerID/modelID" passed to prompts (optional)
	title string // opencode session title for newly-created sessions (spawn name + app)

	mu          sync.Mutex
	sessionID   string
	partKind    map[string]string // opencode partID -> ACP update kind
	msgRole     map[string]string // opencode messageID -> "user"|"assistant"
	sentUser    map[string]bool   // opencode user partID already echoed to the web (once each)
	sentTool    map[string]bool   // opencode tool callID already emitted as a tool_call creation
	recentSelf  []string          // prompt texts WE submitted (web path) — skip echoing those back
	inflightID  int               // node's session/prompt id awaiting turn-end
	hasInflight bool
	turnErr     *OpencodeError    // terminal error seen during the in-flight turn (session.error / msg error)
	usage       *UsageAccumulator // per-turn token usage + cost accumulated from step-finish parts (cat D)
	nextReqID   int
	permByReq   map[int]string // our request id -> opencode permissionID
}

// New builds an adapter for the opencode client, scoping sessions to dir, tagging prompts with model
// (may be empty), and titling newly-created sessions with title (defaults to "spawnery" if empty).
func New(oc *opencode.Client, dir, model, title string) *Adapter {
	if dir == "" {
		dir = "/app"
	}
	if title == "" {
		title = "spawnery"
	}
	return &Adapter{
		oc: oc, dir: dir, model: model, title: title,
		partKind:  map[string]string{},
		msgRole:   map[string]string{},
		sentUser:  map[string]bool{},
		sentTool:  map[string]bool{},
		permByReq: map[int]string{},
		usage:     NewUsageAccumulator(),
	}
}

// Serve runs the ACP agent loop on the given node connection until it ends.
// It starts an SSE pump (opencode events -> ACP notifications) and processes
// node requests/notifications.
func (a *Adapter) Serve(r io.Reader, w io.Writer) error {
	srv := acp.NewServer(r, w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.pumpLoop(ctx, srv)

	for {
		m, err := srv.Read()
		if err != nil {
			return err
		}
		// A message with an id and no method is a response to one of OUR
		// server-initiated requests (session/request_permission). The node pump
		// (our only client) uses integer ids.
		if idn, idok := m.ID.AsInt(); m.Method == "" && idok {
			a.handlePermResponse(idn, m.Result)
			continue
		}
		switch m.Method {
		case "initialize":
			a.handleInitialize(srv, m)
		case "session/new":
			a.handleNewSession(srv, m)
		case "session/prompt":
			a.handlePrompt(srv, m)
		case "session/cancel", "cancel":
			a.mu.Lock()
			sid := a.sessionID
			a.mu.Unlock()
			_ = a.oc.Abort(sid)
		}
	}
}

func (a *Adapter) handleInitialize(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	// opencode serve can be slow to bind on first run (one-time SQLite migration), so poll health
	// rather than failing the handshake immediately. Bounded so we still error well within the node's
	// readyTimeout if opencode never comes up.
	if err := a.waitHealthy(90 * time.Second); err != nil {
		_ = srv.RespondError(id, -32000, "opencode not ready: "+err.Error())
		return
	}
	sid, err := a.oc.DiscoverOrCreateSession(a.dir, a.title)
	if err != nil {
		_ = srv.RespondError(id, -32000, err.Error())
		return
	}
	a.setSession(sid)
	_ = srv.Respond(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
}

func (a *Adapter) handleNewSession(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	a.mu.Lock()
	sid := a.sessionID
	a.mu.Unlock()
	if sid == "" {
		var err error
		if sid, err = a.oc.DiscoverOrCreateSession(a.dir, a.title); err != nil {
			_ = srv.RespondError(id, -32000, err.Error())
			return
		}
		a.setSession(sid)
	}
	_ = srv.Respond(id, map[string]any{"sessionId": sid})
}

// setSession records the opencode session id in memory and to the session file (for spawn-tui).
func (a *Adapter) setSession(sid string) {
	a.mu.Lock()
	a.sessionID = sid
	a.mu.Unlock()
	// Best-effort: the in-container TUI launcher reads this to pin `opencode attach -s <id>`.
	_ = os.WriteFile(sessionFilePath(), []byte(sid), 0o644)
}

func (a *Adapter) handlePrompt(srv *acp.Server, m acp.Message) {
	id, ok := m.ID.AsInt()
	if !ok {
		return
	}
	text := extractPromptText(m.Params)
	a.mu.Lock()
	a.inflightID = id
	a.hasInflight = true
	a.turnErr = nil                 // fresh turn: clear any error carried over from the previous one
	a.usage = NewUsageAccumulator() // fresh turn: reset per-turn token usage + cost (cat D)
	sid := a.sessionID
	// Remember our own (web-originated) prompt text so we don't echo it back as a TUI message —
	// the node already locally-echoes web prompts. Bounded ring.
	a.recentSelf = append(a.recentSelf, text)
	if len(a.recentSelf) > 16 {
		a.recentSelf = a.recentSelf[len(a.recentSelf)-16:]
	}
	a.mu.Unlock()
	if err := a.oc.PromptAsync(sid, text, a.model); err != nil {
		a.mu.Lock()
		a.hasInflight = false
		a.mu.Unlock()
		_ = srv.RespondError(id, -32000, err.Error())
		return
	}
	// The turn-end response (which the node reads as turn-end) is sent when the
	// opencode session.idle event arrives — see onEvent.
}

func (a *Adapter) handlePermResponse(reqID int, result json.RawMessage) {
	a.mu.Lock()
	permID := a.permByReq[reqID]
	delete(a.permByReq, reqID)
	sid := a.sessionID
	a.mu.Unlock()
	if permID == "" {
		return
	}
	var res struct {
		Outcome struct {
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(result, &res)
	_ = a.oc.RespondPermission(sid, permID, ACPOptionIDToOpencodeResponse(res.Outcome.OptionID))
}

// waitHealthy polls opencode /global/health until it succeeds or timeout elapses.
func (a *Adapter) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for {
		if err = a.oc.Health(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// pumpLoop subscribes to opencode /event and reconnects with backoff.
func (a *Adapter) pumpLoop(ctx context.Context, srv *acp.Server) {
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		_ = a.oc.Events(ctx, func(e opencode.RawEvent) { a.onEvent(srv, e) })
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxBackoff {
			backoff = minBackoff // the stream was healthy for a while; reset
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// onEvent translates one opencode event into ACP traffic to the node.
func (a *Adapter) onEvent(srv *acp.Server, e opencode.RawEvent) {
	switch e.Type {
	case "message.updated":
		mu, err := ParseMessageUpdated(e.Properties)
		if err == nil && mu.Info.ID != "" {
			a.mu.Lock()
			a.msgRole[mu.Info.ID] = mu.Info.Role
			// An assistant message can carry a terminal NamedError; record it for the turn-end stopReason.
			if mu.Info.Error != nil && mu.Info.Error.Name != "" {
				a.turnErr = mu.Info.Error
			}
			a.mu.Unlock()
		}
	case "session.error":
		// A session-level NamedError (auth/output-length/aborted/...). Remember it; it shapes the
		// stopReason + structured error sent when this turn ends (session.idle). (cat G)
		se, err := ParseSessionError(e.Properties)
		if err == nil && se.Error != nil && se.Error.Name != "" {
			a.mu.Lock()
			a.turnErr = se.Error
			a.mu.Unlock()
		}
	case "message.part.updated":
		pu, err := ParsePartUpdated(e.Properties)
		if err != nil {
			return
		}
		// step-finish parts: accumulate per-step token usage + cost for the in-flight turn (cat D). They
		// carry no display content, so consume and return.
		if pu.Part.Type == "step-finish" {
			a.mu.Lock()
			if a.usage != nil {
				a.usage.AddStep(pu)
			}
			a.mu.Unlock()
			return
		}
		// Tool parts: emit ACP tool_call (creation, once per callID) + tool_call_update(s) carrying
		// the tool's content/result and rawInput/rawOutput (cats A/I).
		if pu.Part.Type == "tool" {
			a.emitToolPart(srv, pu)
			return
		}
		if kind, ok := PartTypeToACPKind(pu.Part.Type); ok {
			a.mu.Lock()
			a.partKind[pu.Part.ID] = kind
			a.mu.Unlock()
		}
		// Echo a USER message typed in the TUI to the web (as user_message_chunk), unless it's our
		// own web-submitted prompt (the node already echoed that) — deduped by text.
		if pu.Part.Type == "text" && pu.Part.Text != "" {
			a.maybeEchoUser(srv, pu)
		}
	case "message.part.delta":
		d, err := ParsePartDelta(e.Properties)
		if err != nil || d.Field != "text" {
			return
		}
		a.mu.Lock()
		kind := a.partKind[d.PartID]
		a.mu.Unlock()
		if kind == "" {
			kind = "agent_message_chunk"
		}
		_ = srv.Notify("session/update", ACPSessionUpdate(d.SessionID, kind, d.Delta))
	case "todo.updated":
		// The agent's evolving plan/todo list. opencode re-sends the FULL list each time; we forward it
		// as one ACP `plan` session/update and the client replaces its prior plan in place (cat C).
		tu, err := ParseTodoUpdated(e.Properties)
		if err != nil {
			return
		}
		_ = srv.Notify("session/update", TodoUpdateToACP(tu))
	case "permission.asked":
		pa, err := ParsePermissionAsked(e.Properties)
		if err != nil {
			return
		}
		a.mu.Lock()
		a.nextReqID++
		rid := a.nextReqID
		a.permByReq[rid] = pa.ID
		a.mu.Unlock()
		_ = srv.Request(rid, "session/request_permission", map[string]any{
			"options":  PermissionToACPOptions(),
			"toolCall": map[string]any{"title": pa.Permission},
		})
	case "session.idle":
		a.mu.Lock()
		id := a.inflightID
		has := a.hasInflight
		a.hasInflight = false
		te := a.turnErr
		a.turnErr = nil
		var usage *ACPUsage
		if a.usage != nil {
			usage = a.usage.Usage() // per-turn token usage + cost accumulated from step-finish parts (cat D)
		}
		a.mu.Unlock()
		if has {
			// Map opencode's real stop semantics (incl. any NamedError) to an honest ACP stopReason +
			// optional structured error, instead of always collapsing to end_turn. (cat G)
			stop, ei := StopReasonForError(te)
			resp := map[string]any{"stopReason": stop}
			if ei != nil {
				resp["error"] = ei
			}
			if usage != nil {
				resp["usage"] = usage // UNSTABLE/guarded: present only when the turn reported usage
			}
			_ = srv.Respond(id, resp)
		}
	}
}

// emitToolPart translates an opencode ToolPart snapshot into the ACP tool_call/tool_call_update
// notifications, sending a creation update once per callID and progress/result updates thereafter.
func (a *Adapter) emitToolPart(srv *acp.Server, pu PartUpdated) {
	callID := pu.Part.CallID
	if callID == "" {
		return
	}
	a.mu.Lock()
	first := !a.sentTool[callID]
	a.sentTool[callID] = true
	a.mu.Unlock()
	for _, up := range ToolPartUpdates(pu, first) {
		_ = srv.Notify("session/update", up)
	}
}

// maybeEchoUser forwards a user-role text part (typed in the TUI) to the web as user_message_chunk,
// once per part, skipping our own web-submitted prompts (deduped by text against recentSelf).
func (a *Adapter) maybeEchoUser(srv *acp.Server, pu PartUpdated) {
	a.mu.Lock()
	if a.msgRole[pu.Part.MessageID] != "user" { // not a user message (or role not yet known) — skip
		a.mu.Unlock()
		return
	}
	if a.sentUser[pu.Part.ID] { // already echoed this part
		a.mu.Unlock()
		return
	}
	// If this matches a prompt we submitted (web path), consume it and DON'T echo (node already did).
	for i, t := range a.recentSelf {
		if t == pu.Part.Text {
			a.recentSelf = append(a.recentSelf[:i], a.recentSelf[i+1:]...)
			a.sentUser[pu.Part.ID] = true
			a.mu.Unlock()
			return
		}
	}
	a.sentUser[pu.Part.ID] = true
	sid := pu.SessionID
	a.mu.Unlock()
	_ = srv.Notify("session/update", ACPUserUpdate(sid, pu.Part.Text))
}

// extractPromptText concatenates the text content blocks of an ACP session/prompt.
func extractPromptText(params json.RawMessage) string {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	var sb strings.Builder
	for _, b := range p.Prompt {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
