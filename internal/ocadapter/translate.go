// Package ocadapter implements an ACP agent server backed by an opencode
// `serve` instance. It normalizes opencode's HTTP/SSE API to canonical ACP so
// the spawnery node (which speaks spec-ACP) needs no opencode-specific logic.
package ocadapter

import "encoding/json"

// --- opencode event payloads (verified against opencode 1.15.13 /doc) -------
// Every SSE event is {"type":"<name>","properties":{<payload>}}. These structs
// model the `properties` payloads we consume.

// PartUpdated is the payload of a message.part.updated event (full part snapshot).
type PartUpdated struct {
	SessionID string `json:"sessionID"`
	Part      struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"` // "text" | "reasoning" | "tool" | ...
		Text      string `json:"text"`
		// tool-part fields (type=="tool"), unused by other part types (sp-ufz.5).
		CallID string    `json:"callID"` // opencode tool-call id -> ACP toolCallId
		Tool   string    `json:"tool"`   // tool name (e.g. "bash", "read")
		State  ToolState `json:"state"`  // the tool-call state machine snapshot
	} `json:"part"`
}

// ToolState is the (union) `state` of an opencode ToolPart. opencode emits one of
// ToolStatePending/Running/Completed/Error; this flat struct unions their fields (status discriminates).
// Pinned against opencode 1.15.13 /doc (ToolState* schemas).
type ToolState struct {
	Status string          `json:"status"` // pending | running | completed | error
	Input  json.RawMessage `json:"input"`  // the tool's input args -> ACP rawInput
	Output string          `json:"output"` // completed: the tool's text result
	Error  string          `json:"error"`  // error: the failure message
	Title  string          `json:"title"`  // running/completed: a human title for the call
}

// MessageUpdated is the payload of a message.updated event; we use it to learn each message's role
// (user vs assistant) so user-role text parts can be echoed to the web as user_message_chunk.
type MessageUpdated struct {
	SessionID string `json:"sessionID"`
	Info      struct {
		ID   string `json:"id"`
		Role string `json:"role"` // "user" | "assistant"
	} `json:"info"`
}

// ParseMessageUpdated decodes a message.updated properties payload.
func ParseMessageUpdated(raw json.RawMessage) (MessageUpdated, error) {
	var m MessageUpdated
	err := json.Unmarshal(raw, &m)
	return m, err
}

// ACPUserUpdate builds a session/update echoing a user's message text (typed in the TUI) to the web.
func ACPUserUpdate(sessionID, text string) ACPUpdateParams {
	return ACPUpdateParams{
		SessionID: sessionID,
		Update: ACPUpdate{
			SessionUpdate: "user_message_chunk",
			Content:       ACPContent{Type: "text", Text: text},
		},
	}
}

// PartDelta is the payload of a message.part.delta event (streaming increment).
// It carries no part type, so the consumer maps PartID -> type via a prior
// PartUpdated.
type PartDelta struct {
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	PartID    string `json:"partID"`
	Field     string `json:"field"` // e.g. "text"
	Delta     string `json:"delta"`
}

// PermissionAsked is the payload of a permission.asked event (PermissionRequest).
type PermissionAsked struct {
	ID         string `json:"id"`        // per_... -> permissionID
	SessionID  string `json:"sessionID"` // ses_...
	Permission string `json:"permission"`
}

// ParsePartUpdated decodes a message.part.updated properties payload.
func ParsePartUpdated(raw json.RawMessage) (PartUpdated, error) {
	var p PartUpdated
	err := json.Unmarshal(raw, &p)
	return p, err
}

// ParsePartDelta decodes a message.part.delta properties payload.
func ParsePartDelta(raw json.RawMessage) (PartDelta, error) {
	var p PartDelta
	err := json.Unmarshal(raw, &p)
	return p, err
}

// ParsePermissionAsked decodes a permission.asked properties payload.
func ParsePermissionAsked(raw json.RawMessage) (PermissionAsked, error) {
	var p PermissionAsked
	err := json.Unmarshal(raw, &p)
	return p, err
}

// --- canonical ACP shapes ---------------------------------------------------

// ACPUpdateParams is the params of an ACP session/update notification.
type ACPUpdateParams struct {
	SessionID string    `json:"sessionId"`
	Update    ACPUpdate `json:"update"`
}

type ACPUpdate struct {
	SessionUpdate string     `json:"sessionUpdate"`
	Content       ACPContent `json:"content,omitempty"`
}

type ACPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// --- ACP tool-call shapes (cats A/I) ----------------------------------------
// Tool calls use a different `update` shape than text chunks: `content` is an ARRAY of
// ToolCallContent blocks (not a single object), plus toolCallId/title/kind/status and raw I/O.
// They get their own param/update structs so the `content` JSON key doesn't collide with the
// text-chunk ACPUpdate.Content object.

// ACPToolUpdateParams is the params of a tool_call / tool_call_update session/update.
type ACPToolUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    ACPToolUpdate `json:"update"`
}

// ACPToolUpdate is the `update` body of a tool_call (creation) or tool_call_update.
type ACPToolUpdate struct {
	SessionUpdate string           `json:"sessionUpdate"` // "tool_call" | "tool_call_update"
	ToolCallID    string           `json:"toolCallId"`
	Title         string           `json:"title,omitempty"`
	Kind          string           `json:"kind,omitempty"`   // read|edit|execute|search|fetch|other
	Status        string           `json:"status,omitempty"` // pending|in_progress|completed|failed
	Content       []ACPToolContent `json:"content,omitempty"`
	RawInput      json.RawMessage  `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage  `json:"rawOutput,omitempty"`
}

// ACPToolContent is one ToolCallContent block: {"type":"content","content":{"type":"text","text":...}}.
type ACPToolContent struct {
	Type    string     `json:"type"` // "content" (diff/resource variants are later tasks)
	Content ACPContent `json:"content"`
}

// ToolPartUpdates translates one opencode ToolPart snapshot into the ACP session/update(s) to emit.
// On the first snapshot for a callID it emits a tool_call creation (status=pending); for
// running/completed/error states it (also) emits a tool_call_update carrying the ACP status, the
// tool's text result as a content block, and rawInput/rawOutput. Returns nil if the part has no callID.
func ToolPartUpdates(pu PartUpdated, first bool) []ACPToolUpdateParams {
	callID := pu.Part.CallID
	if callID == "" {
		return nil
	}
	sid := pu.SessionID
	st := pu.Part.State
	var out []ACPToolUpdateParams
	if first {
		title := pu.Part.Tool
		if st.Title != "" {
			title = st.Title
		}
		out = append(out, ACPToolCall(sid, callID, title, ToolKind(pu.Part.Tool)))
	}
	if st.Status != "" && st.Status != "pending" {
		out = append(out, ACPToolCallUpdate(sid, callID, st))
	}
	return out
}

// ACPToolCall builds a tool_call creation update.
func ACPToolCall(sessionID, toolCallID, title, kind string) ACPToolUpdateParams {
	return ACPToolUpdateParams{
		SessionID: sessionID,
		Update: ACPToolUpdate{
			SessionUpdate: "tool_call",
			ToolCallID:    toolCallID,
			Title:         title,
			Kind:          kind,
			Status:        "pending",
		},
	}
}

// ACPToolCallUpdate builds a tool_call_update reflecting a running/completed/error ToolState.
func ACPToolCallUpdate(sessionID, toolCallID string, st ToolState) ACPToolUpdateParams {
	u := ACPToolUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    toolCallID,
		Status:        ToolStatusToACP(st.Status),
	}
	if text := toolResultText(st); text != "" {
		u.Content = []ACPToolContent{{Type: "content", Content: ACPContent{Type: "text", Text: text}}}
	}
	if len(st.Input) > 0 {
		u.RawInput = st.Input
	}
	if raw := toolRawOutput(st); raw != nil {
		u.RawOutput = raw
	}
	return ACPToolUpdateParams{SessionID: sessionID, Update: u}
}

// toolResultText is the human-readable result text for a content block (output, or the error message).
func toolResultText(st ToolState) string {
	if st.Output != "" {
		return st.Output
	}
	return st.Error
}

// toolRawOutput is the raw tool output for ACP rawOutput. opencode's output is a plain string, so it is
// JSON-encoded as a string (valid JSON). nil when there is nothing to report.
func toolRawOutput(st ToolState) json.RawMessage {
	s := st.Output
	if s == "" {
		s = st.Error
	}
	if s == "" {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

// ToolStatusToACP maps an opencode ToolState.status to an ACP tool-call status.
func ToolStatusToACP(status string) string {
	switch status {
	case "running":
		return "in_progress"
	case "completed":
		return "completed"
	case "error":
		return "failed"
	default:
		return "pending"
	}
}

// ToolKind maps an opencode tool name to an ACP tool-call kind (best-effort; defaults to "other").
func ToolKind(tool string) string {
	switch tool {
	case "read", "list", "glob":
		return "read"
	case "write", "edit", "patch":
		return "edit"
	case "bash", "shell":
		return "execute"
	case "grep", "search":
		return "search"
	case "webfetch", "fetch":
		return "fetch"
	default:
		return "other"
	}
}

// ACPOption is one permission option offered to the client (canonical ACP kinds).
type ACPOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PartTypeToACPKind maps an opencode part type to the ACP session/update kind
// for streamed content. ok=false for part types we don't stream as text chunks.
func PartTypeToACPKind(partType string) (kind string, ok bool) {
	switch partType {
	case "text":
		return "agent_message_chunk", true
	case "reasoning":
		return "agent_thought_chunk", true
	default:
		return "", false
	}
}

// ACPSessionUpdate builds a session/update for a streamed text/thought chunk.
func ACPSessionUpdate(sessionID, kind, text string) ACPUpdateParams {
	return ACPUpdateParams{
		SessionID: sessionID,
		Update: ACPUpdate{
			SessionUpdate: kind,
			Content:       ACPContent{Type: "text", Text: text},
		},
	}
}

// PermissionToACPOptions returns the canonical four ACP permission options the
// node's pickPermOption selects from (it matches on Kind containing allow/reject).
func PermissionToACPOptions() []ACPOption {
	return []ACPOption{
		{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
		{OptionID: "reject_once", Name: "Reject once", Kind: "reject_once"},
		{OptionID: "reject_always", Name: "Reject always", Kind: "reject_always"},
	}
}

// ACPOptionIDToOpencodeResponse maps the optionId the node selected back to the
// opencode permission response value ("once"|"always"|"reject").
func ACPOptionIDToOpencodeResponse(optionID string) string {
	switch optionID {
	case "allow_once":
		return "once"
	case "allow_always":
		return "always"
	default:
		return "reject"
	}
}
