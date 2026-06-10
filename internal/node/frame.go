package node

import "encoding/json"

// Frame is one ndjson line on the pump<->client wire. Logged conversation frames carry Seq>0
// (user/agent/thought/tool/turn); transient frames carry Seq==0 (perm_request, reset); client->pump
// frames are prompt / perm_response / cancel / set_mode. Kind selects the variant.
//
// Hybrid envelope: the envelope (Seq+Kind) and the original flat scalars are kept byte-stable for the
// simple kinds; the rich kinds carry optional nested payload pointers (populated by tasks sp-ufz.5..13).
type Frame struct {
	// envelope (unchanged)
	Seq  int64  `json:"seq,omitempty"`
	Kind string `json:"kind"`

	// existing scalars (unchanged) — order preserved so existing kinds stay byte-stable
	Text    string `json:"text,omitempty"`    // user/agent/thought/prompt
	ToolID  string `json:"toolId,omitempty"`  // tool
	Title   string `json:"title,omitempty"`   // tool / perm_request
	Status  string `json:"status,omitempty"`  // tool
	State   string `json:"state,omitempty"`   // turn: busy|idle
	Queued  int    `json:"queued,omitempty"`  // turn
	ReqID   string `json:"reqId,omitempty"`   // perm_request / perm_response
	FromSeq int64  `json:"fromSeq,omitempty"` // reset

	// new optional payloads — appended after existing fields so existing kinds are wire-identical
	Tool     *ToolPayload `json:"tool,omitempty"`     // enriches kind="tool"          (sp-ufz.5/.6)
	Plan     []PlanEntry  `json:"plan,omitempty"`     // kind="plan" replace-in-place   (sp-ufz.9)
	Usage    *Usage       `json:"usage,omitempty"`    // rides kind="turn"             (sp-ufz.10)
	Reason   string       `json:"reason,omitempty"`   // kind="turn"                   (sp-ufz.7)
	Error    *ErrorInfo   `json:"error,omitempty"`    // kind="turn"                   (sp-ufz.7)
	Cmds     []Command    `json:"cmds,omitempty"`     // kind="commands"               (sp-ufz.11)
	Mode     *ModePayload `json:"mode,omitempty"`     // kind="mode"                   (sp-ufz.12)
	Options  []PermOption `json:"options,omitempty"`  // kind="perm_request"           (sp-ufz.8)
	OptionID string       `json:"optionId,omitempty"` // kind="perm_response"          (sp-ufz.8)
	ModeID   string       `json:"modeId,omitempty"`   // kind="set_mode" (client->node) (sp-ufz.12)
}

// ToolPayload enriches kind="tool" with content/result blocks, a file diff, and raw I/O (cats A/B/I).
type ToolPayload struct {
	Content   []ContentBlock  `json:"content,omitempty"`
	Diff      *Diff           `json:"diff,omitempty"`
	RawInput  json.RawMessage `json:"rawInput,omitempty"`
	RawOutput json.RawMessage `json:"rawOutput,omitempty"`
}

// ContentBlock is one tool content/result block. Extend with image/resource variants later.
type ContentBlock struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// Diff is a file edit artifact.
type Diff struct {
	Path    string `json:"path,omitempty"`
	OldText string `json:"oldText,omitempty"`
	NewText string `json:"newText,omitempty"`
}

// PlanEntry is one agent plan/todo item.
type PlanEntry struct {
	Content  string `json:"content,omitempty"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
}

// Usage is the per-turn token breakdown (UNSTABLE in ACP — guarded by presence by consumers).
type Usage struct {
	Input   int      `json:"input,omitempty"`
	Output  int      `json:"output,omitempty"`
	Cached  int      `json:"cached,omitempty"`
	Thought int      `json:"thought,omitempty"`
	Total   int      `json:"total,omitempty"`
	Cost    *float64 `json:"cost,omitempty"`
}

// ErrorInfo is a structured turn error (from the opencode NamedError union).
type ErrorInfo struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Command is an advertised slash command.
type Command struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	InputHint   string `json:"inputHint,omitempty"`
}

// PermOption is one permission choice (kind: allow_once|allow_always|reject_once|reject_always|...).
type PermOption struct {
	OptionID string `json:"optionId,omitempty"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// Mode is one selectable session mode.
type Mode struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ModePayload is the current mode plus the available set.
type ModePayload struct {
	Current   string `json:"current,omitempty"`
	Available []Mode `json:"available,omitempty"`
}

func encodeFrame(f Frame) []byte {
	b, err := json.Marshal(f)
	if err != nil {
		// A malformed raw payload (e.g. invalid Tool.RawInput/RawOutput) makes Marshal fail and
		// return nil bytes. Returning append(nil, '\n') would emit a bare newline / empty frame and
		// silently corrupt the ndjson stream. Surface the failure via the package logger and emit
		// nothing instead (callers send a nil slice, i.e. no bytes, rather than a broken frame).
		logErr("encodeFrame kind="+f.Kind, err)
		return nil
	}
	return append(b, '\n')
}

func decodeFrame(line []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, err
	}
	return f, nil
}
