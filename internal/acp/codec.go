// Package acp implements the minimal slice of the Agent Client Protocol
// (JSON-RPC 2.0 over stdio, newline-delimited) that the slice needs.
package acp

import (
	"bufio"
	"encoding/json"
	"io"
)

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func WriteMessage(w io.Writer, m Message) error {
	if m.JSONRPC == "" {
		m.JSONRPC = "2.0"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

type Reader struct{ sc *bufio.Scanner }

func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // allow large messages
	return &Reader{sc: sc}
}

func (r *Reader) ReadMessage() (Message, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, io.EOF
	}
	var m Message
	if err := json.Unmarshal(r.sc.Bytes(), &m); err != nil {
		return Message{}, err
	}
	return m, nil
}
