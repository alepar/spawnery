// Package ocadapter implements an ACP agent server backed by an opencode
// `serve` instance. It normalizes opencode's HTTP/SSE API to canonical ACP so
// the spawnery node (which speaks spec-ACP) needs no opencode-specific logic.
package ocadapter

import (
	"encoding/json"
	"strings"
)

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
		// step-finish part fields (type=="step-finish"): per-step token usage + cost (cat D, sp-ufz.10).
		Cost   float64    `json:"cost"`
		Tokens TokenUsage `json:"tokens"`
	} `json:"part"`
}

// TokenUsage is opencode's per-step token breakdown on a step-finish part (cat D). cache splits into
// read/write (we fold both into ACP `cached`); reasoning is the thinking-token count. Pinned against
// opencode 1.15.13 /doc (StepFinishPart.tokens schema).
type TokenUsage struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
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
// (user vs assistant) so user-role text parts can be echoed to the web as user_message_chunk, and to
// pick up an assistant message's terminal error (the same NamedError union as session.error) for cat G.
type MessageUpdated struct {
	SessionID string `json:"sessionID"`
	Info      struct {
		ID    string         `json:"id"`
		Role  string         `json:"role"` // "user" | "assistant"
		Error *OpencodeError `json:"error"`
	} `json:"info"`
}

// OpencodeError models one opencode NamedError (the discriminated union surfaced on a session.error
// event and on an assistant message's `error` field). opencode's known variants include
// ProviderAuthError, MessageOutputLengthError, MessageAbortedError, and UnknownError; Data carries an
// optional human message. We match on Name leniently (substring) so adjacent/renamed variants still map.
type OpencodeError struct {
	Name string `json:"name"`
	Data struct {
		Message    string `json:"message"`
		ProviderID string `json:"providerID"`
	} `json:"data"`
}

// SessionError is the payload of a session.error event: an optional sessionID and the NamedError.
type SessionError struct {
	SessionID string         `json:"sessionID"`
	Error     *OpencodeError `json:"error"`
}

// ParseSessionError decodes a session.error properties payload.
func ParseSessionError(raw json.RawMessage) (SessionError, error) {
	var s SessionError
	err := json.Unmarshal(raw, &s)
	return s, err
}

// ACPError is the structured turn error carried (alongside stopReason) on the session/prompt response.
// It mirrors the node-side ErrorInfo so the pump can drop it straight onto the turn Frame (cat G).
type ACPError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// StopReasonForError maps an opencode NamedError to an ACP stop reason plus an optional structured
// error. Cancellation and an output-length cap are honest stop reasons (`cancelled` / `max_tokens`)
// that carry NO error object; auth/context/structured/unknown failures keep a best-fit stop reason but
// ALSO surface a structured error (message) so the client can show what actually went wrong. A nil
// error (the common case) is a clean `end_turn`.
func StopReasonForError(e *OpencodeError) (stop string, errInfo *ACPError) {
	if e == nil || e.Name == "" {
		return "end_turn", nil
	}
	n := strings.ToLower(e.Name)
	msg := e.Data.Message
	switch {
	case strings.Contains(n, "abort") || strings.Contains(n, "cancel"):
		return "cancelled", nil // a deliberate interrupt is not an error
	case strings.Contains(n, "outputlength") || strings.Contains(n, "output_length"):
		return "max_tokens", nil // hit the model's output cap — reason says it all
	case strings.Contains(n, "context") || strings.Contains(n, "overflow"):
		if msg == "" {
			msg = "context window exceeded"
		}
		return "max_tokens", &ACPError{Message: msg}
	case strings.Contains(n, "refus"):
		return "refusal", &ACPError{Message: msg}
	default: // auth, structured-output, unknown, ... — no clean stop reason; report honestly as an error
		if msg == "" {
			msg = e.Name
		}
		return "end_turn", &ACPError{Message: msg}
	}
}

// --- per-turn token usage + cost (cat D, sp-ufz.10) -------------------------
// opencode emits a `step-finish` message part per LLM step; an assistant turn may run several. Each part
// carries that step's token usage + cost (priced via models.dev). We accumulate them across the turn and
// attach the total as `usage` on the session/prompt response (PromptResponse.usage) at turn end. UNSTABLE
// in ACP — guarded by presence: an agent (e.g. goose) that emits no step-finish parts -> no usage field.

// ACPUsage is the per-turn token breakdown carried on the session/prompt response (PromptResponse.usage).
// It mirrors the node-side Usage so the pump drops it straight onto the turn Frame. omitempty everywhere
// so an absent/zero field is omitted and Cost is a pointer (absent unless the turn was actually priced).
type ACPUsage struct {
	Input   int      `json:"input,omitempty"`
	Output  int      `json:"output,omitempty"`
	Cached  int      `json:"cached,omitempty"`
	Thought int      `json:"thought,omitempty"`
	Total   int      `json:"total,omitempty"`
	Cost    *float64 `json:"cost,omitempty"`
}

// UsageAccumulator sums per-step opencode token usage + cost across one assistant turn. Steps are keyed
// by partID so a re-sent message.part.updated snapshot of the same step overwrites rather than
// double-counts. Use a fresh accumulator per prompt (reset at turn start).
type UsageAccumulator struct {
	steps map[string]stepContribution
}

// stepContribution is one step-finish part's contribution to the turn total.
type stepContribution struct {
	input, output, cached, thought int
	cost                           float64
}

// NewUsageAccumulator returns an empty per-turn usage accumulator.
func NewUsageAccumulator() *UsageAccumulator {
	return &UsageAccumulator{steps: map[string]stepContribution{}}
}

// AddStep records a step-finish part's usage (keyed by part id; a repeat snapshot overwrites). A part
// that is not a step-finish, or carries no id, is ignored — so callers can hand it every part safely.
func (u *UsageAccumulator) AddStep(pu PartUpdated) {
	if pu.Part.Type != "step-finish" || pu.Part.ID == "" {
		return
	}
	t := pu.Part.Tokens
	u.steps[pu.Part.ID] = stepContribution{
		input:   t.Input,
		output:  t.Output,
		cached:  t.Cache.Read + t.Cache.Write,
		thought: t.Reasoning,
		cost:    pu.Part.Cost,
	}
}

// Usage returns the accumulated per-turn usage, or nil when nothing meaningful was reported (no
// step-finish parts, or only all-zero/unpriced ones). Returning nil keeps the `usage` field absent so a
// non-reporting agent never renders a zero-token badge or a misleading $0.00 (UNSTABLE/guarded).
func (u *UsageAccumulator) Usage() *ACPUsage {
	var acc ACPUsage
	var cost float64
	for _, s := range u.steps {
		acc.Input += s.input
		acc.Output += s.output
		acc.Cached += s.cached
		acc.Thought += s.thought
		cost += s.cost
	}
	// Carry cost only when the turn was actually priced (>0); a zero cost is indistinguishable from
	// "unpriced" and would render a misleading $0.00.
	if cost > 0 {
		c := cost
		acc.Cost = &c
	}
	acc.Total = acc.Input + acc.Output
	if acc.Input == 0 && acc.Output == 0 && acc.Cached == 0 && acc.Thought == 0 && acc.Cost == nil {
		return nil // an empty/all-zero turn carries no usage
	}
	return &acc
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

// --- agent plan / todos (cat C) ---------------------------------------------
// opencode surfaces the agent's evolving todo list as a `todo.updated` event whose properties carry
// the full todo list for the session (replace-in-place: each event is the complete current list, not a
// delta). Each todo has content + a status (pending|in_progress|completed|cancelled) and a priority
// (high|medium|low). We normalize the whole list to one ACP `plan` session/update.

// TodoUpdated is the payload of a todo.updated event: the session id plus the FULL current todo list.
type TodoUpdated struct {
	SessionID string `json:"sessionID"`
	Todos     []Todo `json:"todos"`
}

// Todo is one opencode todo entry.
type Todo struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`   // pending | in_progress | completed | cancelled
	Priority string `json:"priority"` // high | medium | low
}

// ParseTodoUpdated decodes a todo.updated properties payload.
func ParseTodoUpdated(raw json.RawMessage) (TodoUpdated, error) {
	var t TodoUpdated
	err := json.Unmarshal(raw, &t)
	return t, err
}

// ACPPlanParams is the params of a `plan` session/update notification (cat C).
type ACPPlanParams struct {
	SessionID string        `json:"sessionId"`
	Update    ACPPlanUpdate `json:"update"`
}

// ACPPlanUpdate is the `update` body of a plan session/update. entries is the FULL current plan; the
// client replaces its prior plan in place (latest supersedes), it never appends.
type ACPPlanUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // always "plan"
	Entries       []ACPPlanEntry `json:"entries"`
}

// ACPPlanEntry is one plan/todo item (ACP plan entry shape).
type ACPPlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"` // high | medium | low
	Status   string `json:"status"`   // pending | in_progress | completed
}

// TodoUpdateToACP normalizes an opencode todo list into the ACP `plan` session/update to emit. The
// whole list rides one notification (replace-in-place). Empty/garbled entries are dropped (no content).
func TodoUpdateToACP(tu TodoUpdated) ACPPlanParams {
	entries := make([]ACPPlanEntry, 0, len(tu.Todos))
	for _, td := range tu.Todos {
		if td.Content == "" {
			continue // a plan item with no text carries no information
		}
		entries = append(entries, ACPPlanEntry{
			Content:  td.Content,
			Priority: todoPriorityToACP(td.Priority),
			Status:   todoStatusToACP(td.Status),
		})
	}
	return ACPPlanParams{
		SessionID: tu.SessionID,
		Update:    ACPPlanUpdate{SessionUpdate: "plan", Entries: entries},
	}
}

// todoStatusToACP maps an opencode todo status to an ACP plan-entry status. opencode's `cancelled`
// has no ACP equivalent; it collapses to `completed` (the item is no longer outstanding). Unknown/empty
// defaults to `pending`.
func todoStatusToACP(status string) string {
	switch status {
	case "in_progress":
		return "in_progress"
	case "completed", "cancelled":
		return "completed"
	default:
		return "pending"
	}
}

// todoPriorityToACP maps an opencode todo priority to an ACP priority, defaulting empty/unknown to
// `medium` so the ACP entry always carries a valid priority.
func todoPriorityToACP(priority string) string {
	switch priority {
	case "high", "low", "medium":
		return priority
	default:
		return "medium"
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

// ACPToolContent is one ToolCallContent block. It is a small union keyed on Type:
//   - "content": a text/result block carried under Content ({"type":"content","content":{"type":"text",...}}).
//   - "diff":    a file-edit block ({"type":"diff","path","oldText","newText"}) (cat B).
//
// Content is a pointer so a diff block omits it, and the diff fields are omitempty so a content block
// omits them — each block serializes to exactly its variant's shape.
type ACPToolContent struct {
	Type    string      `json:"type"`              // "content" | "diff"
	Content *ACPContent `json:"content,omitempty"` // type=="content"
	Path    string      `json:"path,omitempty"`    // type=="diff": the edited file path
	OldText string      `json:"oldText,omitempty"` // type=="diff": pre-edit text ("" for a new file)
	NewText string      `json:"newText,omitempty"` // type=="diff": post-edit text
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
		out = append(out, ACPToolCallUpdate(sid, callID, pu.Part.Tool, st))
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

// ACPToolCallUpdate builds a tool_call_update reflecting a running/completed/error ToolState. tool is
// the opencode tool name, used to recognize file-modifying tools and attach a diff content block (cat B).
func ACPToolCallUpdate(sessionID, toolCallID, tool string, st ToolState) ACPToolUpdateParams {
	u := ACPToolUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    toolCallID,
		Status:        ToolStatusToACP(st.Status),
	}
	if text := toolResultText(st); text != "" {
		u.Content = append(u.Content, ACPToolContent{Type: "content", Content: &ACPContent{Type: "text", Text: text}})
	}
	if d := toolDiffBlock(tool, st); d != nil {
		u.Content = append(u.Content, *d)
	}
	if len(st.Input) > 0 {
		u.RawInput = st.Input
	}
	if raw := toolRawOutput(st); raw != nil {
		u.RawOutput = raw
	}
	return ACPToolUpdateParams{SessionID: sessionID, Update: u}
}

// toolDiffBlock returns an ACP diff content block for a file-modifying (edit/write/patch) tool,
// extracted faithfully from the tool's input args — opencode's edit input carries filePath +
// oldString/newString and write carries filePath + content. Returns nil for non-edit tools or when no
// file path is present (best-effort: it never fabricates old/new text).
func toolDiffBlock(tool string, st ToolState) *ACPToolContent {
	if ToolKind(tool) != "edit" {
		return nil
	}
	var in struct {
		FilePath  string `json:"filePath"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
		Content   string `json:"content"` // write tool: full file body
	}
	if len(st.Input) > 0 {
		_ = json.Unmarshal(st.Input, &in)
	}
	if in.FilePath == "" {
		return nil
	}
	newText := in.NewString
	if newText == "" {
		newText = in.Content // write overwrites with the whole content; oldString is empty
	}
	return &ACPToolContent{Type: "diff", Path: in.FilePath, OldText: in.OldString, NewText: newText}
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

// PermissionToACPOptions returns the ACP permission options that faithfully mirror opencode's real
// permission model. opencode answers a permission with once|always|reject (RespondPermission) — it has
// a persistent ALLOW (`always`) but NO persistent reject, so we offer three honestly-kinded options
// rather than a fabricated reject_always that would collapse to the same `reject` as reject_once.
func PermissionToACPOptions() []ACPOption {
	return []ACPOption{
		{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
		{OptionID: "reject_once", Name: "Reject", Kind: "reject_once"},
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
