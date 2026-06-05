package ocadapter

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/opencode"
)

// Adapter presents a canonical-ACP agent to the node, backed by an opencode
// server. One Adapter serves one node connection; the spawn maps to a single
// opencode session (discover-or-create, resume-safe).
type Adapter struct {
	oc    *opencode.Client
	dir   string // the spawn working dir; scopes session discovery
	model string // "providerID/modelID" passed to prompts (optional)

	mu          sync.Mutex
	sessionID   string
	partKind    map[string]string // opencode partID -> ACP update kind
	inflightID  int               // node's session/prompt id awaiting turn-end
	hasInflight bool
	nextReqID   int
	permByReq   map[int]string // our request id -> opencode permissionID
}

// New builds an adapter for the opencode client, scoping sessions to dir and
// tagging prompts with model (may be empty).
func New(oc *opencode.Client, dir, model string) *Adapter {
	if dir == "" {
		dir = "/app"
	}
	return &Adapter{
		oc: oc, dir: dir, model: model,
		partKind:  map[string]string{},
		permByReq: map[int]string{},
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
		// server-initiated requests (session/request_permission).
		if m.Method == "" && m.ID != nil {
			a.handlePermResponse(*m.ID, m.Result)
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
	if m.ID == nil {
		return
	}
	if err := a.oc.Health(); err != nil {
		_ = srv.RespondError(*m.ID, -32000, "opencode not ready: "+err.Error())
		return
	}
	sid, err := a.oc.DiscoverOrCreateSession(a.dir, "spawnery")
	if err != nil {
		_ = srv.RespondError(*m.ID, -32000, err.Error())
		return
	}
	a.mu.Lock()
	a.sessionID = sid
	a.mu.Unlock()
	_ = srv.Respond(*m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
}

func (a *Adapter) handleNewSession(srv *acp.Server, m acp.Message) {
	if m.ID == nil {
		return
	}
	a.mu.Lock()
	sid := a.sessionID
	a.mu.Unlock()
	if sid == "" {
		var err error
		if sid, err = a.oc.DiscoverOrCreateSession(a.dir, "spawnery"); err != nil {
			_ = srv.RespondError(*m.ID, -32000, err.Error())
			return
		}
		a.mu.Lock()
		a.sessionID = sid
		a.mu.Unlock()
	}
	_ = srv.Respond(*m.ID, map[string]any{"sessionId": sid})
}

func (a *Adapter) handlePrompt(srv *acp.Server, m acp.Message) {
	if m.ID == nil {
		return
	}
	text := extractPromptText(m.Params)
	a.mu.Lock()
	a.inflightID = *m.ID
	a.hasInflight = true
	sid := a.sessionID
	a.mu.Unlock()
	if err := a.oc.PromptAsync(sid, text, a.model); err != nil {
		a.mu.Lock()
		a.hasInflight = false
		a.mu.Unlock()
		_ = srv.RespondError(*m.ID, -32000, err.Error())
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
	case "message.part.updated":
		pu, err := ParsePartUpdated(e.Properties)
		if err != nil {
			return
		}
		if kind, ok := PartTypeToACPKind(pu.Part.Type); ok {
			a.mu.Lock()
			a.partKind[pu.Part.ID] = kind
			a.mu.Unlock()
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
		a.mu.Unlock()
		if has {
			_ = srv.Respond(id, map[string]any{"stopReason": "end_turn"})
		}
	}
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
