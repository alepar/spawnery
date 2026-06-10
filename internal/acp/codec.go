// Package acp implements the minimal slice of the Agent Client Protocol
// (JSON-RPC 2.0 over stdio, newline-delimited) that the slice needs.
package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RawID is a JSON-RPC 2.0 request id. The spec allows the id to be a string OR a
// number (or null), and clients pick either: our own Client/Server and the node
// pump use integers, but nori (the Rust ACP TUI, sp-9xr.12.2) uses string UUIDs.
// Storing the id as raw JSON lets acpmux echo a client's id back VERBATIM
// regardless of type, while the integer helpers below keep all our
// integer-keyed bookkeeping (waiters, in-flight prompt ids) working unchanged.
type RawID json.RawMessage

// IntID builds a RawID from an integer id (our outbound requests/responses).
func IntID(n int) *RawID {
	r := RawID(strconv.Itoa(n))
	return &r
}

// AsInt parses the id as an integer, reporting ok=false for string/null ids.
// Used by the integer-keyed upstream bookkeeping (handshake waiters, in-flight
// prompt correlation) — those ids are always ours, hence always integers.
func (r *RawID) AsInt() (int, bool) {
	if r == nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(*r)))
	if err != nil {
		return 0, false
	}
	return n, true
}

func (r RawID) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

func (r *RawID) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *RawID          `json:"id,omitempty"`
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
