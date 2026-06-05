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
		ID   string `json:"id"`
		Type string `json:"type"` // "text" | "reasoning" | "tool" | ...
		Text string `json:"text"`
	} `json:"part"`
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
