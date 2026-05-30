package acp

import (
	"encoding/json"
	"fmt"
	"io"
)

// Client speaks ACP (JSON-RPC 2.0 over stdio) over an io.Reader/io.Writer pair.
type Client struct {
	w   io.Writer
	r   *Reader
	nid int
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
	if err := WriteMessage(c.w, Message{ID: &id, Method: method, Params: raw}); err != nil {
		return Message{}, err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return Message{}, err
		}
		if m.ID != nil && *m.ID == id {
			if m.Error != nil {
				return Message{}, fmt.Errorf("acp %s error %d: %s", method, m.Error.Code, m.Error.Message)
			}
			return m, nil
		}
		// ignore notifications (no id) during simple calls
	}
}

// Initialize sends the ACP initialize handshake.
func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]string{"protocolVersion": "slice-0"})
	return err
}

// NewSession opens a new agent session rooted at cwd.
func (c *Client) NewSession(cwd string) error {
	_, err := c.call("session/new", map[string]string{"cwd": cwd})
	return err
}

// Prompt sends a session/prompt and invokes onChunk for each streamed
// session/update notification until the matching response arrives.
func (c *Client) Prompt(text string, onChunk func(string)) error {
	id := c.next()
	if err := WriteMessage(c.w, Message{
		ID:     &id,
		Method: "session/prompt",
		Params: mustJSON(map[string]string{"text": text}),
	}); err != nil {
		return err
	}
	for {
		m, err := c.r.ReadMessage()
		if err != nil {
			return err
		}
		if m.Method == "session/update" {
			var u struct {
				Chunk string `json:"chunk"`
			}
			if json.Unmarshal(m.Params, &u) == nil && u.Chunk != "" {
				onChunk(u.Chunk)
			}
			continue
		}
		if m.ID != nil && *m.ID == id {
			if m.Error != nil {
				return fmt.Errorf("acp session/prompt error %d: %s", m.Error.Code, m.Error.Message)
			}
			return nil
		}
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
