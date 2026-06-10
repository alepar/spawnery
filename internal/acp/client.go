package acp

import (
	"encoding/json"
	"fmt"
	"io"
)

// Client speaks the real Agent Client Protocol (JSON-RPC 2.0 over stdio,
// newline-delimited) over an io.Reader/io.Writer pair. It implements the
// minimal slice the spawnlet needs: initialize, session/new, session/prompt,
// and streamed session/update notifications carrying agent_message_chunk
// content blocks. See https://agentclientprotocol.com/.
type Client struct {
	w         io.Writer
	r         *Reader
	nid       int
	sessionID string
}

// NewClient creates an ACP client reading from r and writing to w.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{w: w, r: NewReader(r)}
}

func (c *Client) next() int { c.nid++; return c.nid }

// call sends a request and reads messages until the matching response id arrives.
// Notifications (no id) received while waiting are silently dropped.
func (c *Client) call(method string, params any) (Message, error) {
	id := c.next()
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	if err := WriteMessage(c.w, Message{ID: IntID(id), Method: method, Params: raw}); err != nil {
		return Message{}, err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return Message{}, err
		}
		if n, ok := m.ID.AsInt(); ok && n == id {
			if m.Error != nil {
				return Message{}, fmt.Errorf("acp %s error %d: %s", method, m.Error.Code, m.Error.Message)
			}
			return m, nil
		}
		// ignore notifications (no id) during simple calls
	}
}

// Initialize performs the real ACP initialize handshake. protocolVersion is a
// numeric version (ACP uses an unsigned 16-bit integer) and we advertise an
// empty set of client capabilities (the slice exposes no fs/terminal).
func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	})
	return err
}

// NewSession opens a new agent session rooted at cwd with no MCP servers and
// records the returned sessionId for subsequent prompts.
func (c *Client) NewSession(cwd string) error {
	m, err := c.call("session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		return err
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(m.Result, &res); err != nil {
		return fmt.Errorf("acp session/new: bad result: %w", err)
	}
	c.sessionID = res.SessionID
	return nil
}

// promptParams is the real ACP session/prompt request: a sessionId plus a
// prompt array of content blocks.
type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// sessionUpdateParams is the real ACP session/update notification payload.
type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string       `json:"sessionUpdate"`
		Content       contentBlock `json:"content"`
	} `json:"update"`
}

// Prompt sends a real ACP session/prompt (sessionId + a single text content
// block) and invokes onChunk for each streamed agent_message_chunk update until
// the matching response (carrying a stopReason) arrives.
func (c *Client) Prompt(text string, onChunk func(string)) error {
	id := c.next()
	params := promptParams{
		SessionID: c.sessionID,
		Prompt:    []contentBlock{{Type: "text", Text: text}},
	}
	if err := WriteMessage(c.w, Message{
		ID:     IntID(id),
		Method: "session/prompt",
		Params: mustJSON(params),
	}); err != nil {
		return err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return err
		}
		if m.Method == "session/update" {
			var u sessionUpdateParams
			if json.Unmarshal(m.Params, &u) == nil &&
				u.Update.SessionUpdate == "agent_message_chunk" &&
				u.Update.Content.Text != "" {
				onChunk(u.Update.Content.Text)
			}
			continue
		}
		if n, ok := m.ID.AsInt(); ok && n == id {
			if m.Error != nil {
				return fmt.Errorf("acp session/prompt error %d: %s", m.Error.Code, m.Error.Message)
			}
			return nil
		}
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
