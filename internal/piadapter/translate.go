// Package piadapter implements an ACP agent server backed by a `pi --mode rpc`
// child process. It normalizes pi's LF-only JSONL event stream to canonical ACP
// so the spawnery node (which speaks spec-ACP) needs no pi-specific logic. It
// mirrors internal/ocadapter, but the backend is a child process over stdio
// rather than an HTTP/SSE server.
package piadapter

import (
	"encoding/json"
	"strings"
)

// --- pi --mode rpc commands (adapter -> pi stdin) ---------------------------
// Commands are single LF-terminated JSON objects discriminated by "type".

// promptCommand drives one assistant turn with the given user text.
type promptCommand struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// abortCommand interrupts the in-flight turn.
type abortCommand struct {
	Type string `json:"type"`
}

// PromptCommand builds the pi `prompt` command for the given user text.
func PromptCommand(text string) any { return promptCommand{Type: "prompt", Message: text} }

// AbortCommand builds the pi `abort` command.
func AbortCommand() any { return abortCommand{Type: "abort"} }

// --- pi --mode rpc events (pi stdout -> adapter) ----------------------------
// Every event is a single LF-terminated JSON object discriminated by "type".
// This flat struct unions the fields we consume across event types (mirrors
// ocadapter's lenient ToolState union); the discriminator selects which apply.
// Grounded in Spike S1 (v0.80.2) observed lifecycle + pi rpc.md documentation.
type Event struct {
	Type string `json:"type"`

	// message_update: a streaming content increment.
	Delta   string `json:"delta"`   // the text increment
	Channel string `json:"channel"` // "" | "thinking"/"reasoning" => agent_thought_chunk

	// tool_execution_start|update|end: tool call lifecycle.
	// "name" is dual-use: tool name (start) and error class (extension_error).
	ToolCallID string          `json:"toolCallId"`
	Name       string          `json:"name"`    // tool name OR error class
	Input      json.RawMessage `json:"input"`   // tool args (start)
	Output     string          `json:"output"`  // tool result (end)
	IsError    bool            `json:"isError"` // true = tool errored

	// turn_end: per-turn usage.
	Usage *PiUsage `json:"usage"`

	// extension_error: human-readable detail (Name = error class above).
	Message string `json:"message"`
}

// PiUsage is pi's per-turn token report (carried on turn_end).
// Field names confirmed against pi rpc.md (v0.80.2).
type PiUsage struct {
	Input     int `json:"inputTokens"`
	Output    int `json:"outputTokens"`
	Cached    int `json:"cachedTokens"`
	Reasoning int `json:"reasoningTokens"`
}

// PiError models a pi terminal-failure event (extension_error). The Name field
// is the error class (e.g. "ProviderAuthError", "MessageAborted"); Message is
// the optional human detail.
type PiError struct {
	Name    string
	Message string
}

// StopReasonForError maps a pi error to an ACP stop reason plus an optional
// structured error. Cancellation and output-length cap are honest stop reasons
// that carry NO error object; auth/unknown failures keep end_turn but ALSO
// surface a structured error. A nil PiError (the clean case) is end_turn.
func StopReasonForError(e *PiError) (stop string, errInfo *ACPError) {
	if e == nil || e.Name == "" {
		return "end_turn", nil
	}
	n := strings.ToLower(e.Name)
	msg := e.Message
	switch {
	case strings.Contains(n, "abort") || strings.Contains(n, "cancel"):
		return "cancelled", nil
	case strings.Contains(n, "outputlength") || strings.Contains(n, "output_length"):
		return "max_tokens", nil
	case strings.Contains(n, "context") || strings.Contains(n, "overflow"):
		if msg == "" {
			msg = "context window exceeded"
		}
		return "max_tokens", &ACPError{Message: msg}
	default:
		if msg == "" {
			msg = e.Name
		}
		return "end_turn", &ACPError{Message: msg}
	}
}

// ACPError is the structured turn error carried alongside stopReason on the
// session/prompt response.
type ACPError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ACPUsage is the per-turn token breakdown on the session/prompt response.
type ACPUsage struct {
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
	Cached  int `json:"cached,omitempty"`
	Thought int `json:"thought,omitempty"`
	Total   int `json:"total,omitempty"`
}

// PiUsageToACP converts a pi turn_end usage to the ACP usage shape. Returns nil
// when all counts are zero (a non-reporting turn carries no usage field).
func PiUsageToACP(u *PiUsage) *ACPUsage {
	if u == nil {
		return nil
	}
	a := &ACPUsage{
		Input:   u.Input,
		Output:  u.Output,
		Cached:  u.Cached,
		Thought: u.Reasoning,
		Total:   u.Input + u.Output,
	}
	if a.Input == 0 && a.Output == 0 && a.Cached == 0 && a.Thought == 0 {
		return nil
	}
	return a
}

// --- canonical ACP update shapes -------------------------------------------

// ACPUpdateParams is the params of an ACP session/update notification.
type ACPUpdateParams struct {
	SessionID string    `json:"sessionId"`
	Update    ACPUpdate `json:"update"`
}

// ACPUpdate is the update body of a text/thought chunk session/update.
type ACPUpdate struct {
	SessionUpdate string     `json:"sessionUpdate"`
	Content       ACPContent `json:"content,omitempty"`
}

// ACPContent is a simple text content block.
type ACPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ACPSessionUpdate builds a session/update for a streamed text or thought chunk.
func ACPSessionUpdate(sessionID, kind, text string) ACPUpdateParams {
	return ACPUpdateParams{
		SessionID: sessionID,
		Update: ACPUpdate{
			SessionUpdate: kind,
			Content:       ACPContent{Type: "text", Text: text},
		},
	}
}

// --- ACP tool-call shapes ---------------------------------------------------

// ACPToolUpdateParams is the params of a tool_call / tool_call_update session/update.
type ACPToolUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    ACPToolUpdate `json:"update"`
}

// ACPToolUpdate is the update body of a tool_call creation or tool_call_update.
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

// ACPToolContent is one content block in a tool_call_update.
type ACPToolContent struct {
	Type    string      `json:"type"`
	Content *ACPContent `json:"content,omitempty"`
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

// ACPToolCallUpdate builds a tool_call_update for a completed or failed tool execution.
func ACPToolCallUpdate(sessionID, toolCallID, output string, isError bool, rawInput json.RawMessage) ACPToolUpdateParams {
	status := "completed"
	if isError {
		status = "failed"
	}
	u := ACPToolUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    toolCallID,
		Status:        status,
	}
	if output != "" {
		u.Content = []ACPToolContent{{
			Type:    "content",
			Content: &ACPContent{Type: "text", Text: output},
		}}
		b, _ := json.Marshal(output)
		u.RawOutput = b
	}
	if len(rawInput) > 0 {
		u.RawInput = rawInput
	}
	return ACPToolUpdateParams{SessionID: sessionID, Update: u}
}

// ToolKind maps a pi tool name to an ACP tool-call kind (best-effort; defaults
// to "other"). Mirrors ocadapter.ToolKind exactly.
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
