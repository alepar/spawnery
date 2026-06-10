package acp

import (
	"encoding/json"
	"io"
	"sync"
)

// Server is the agent side of ACP: it reads client requests/notifications and
// writes responses and notifications, reusing the shared codec. It is the
// counterpart to Client and is used by the opencode adapter to present a
// canonical-ACP agent to the node.
//
// Writes are serialized by wmu so the adapter's SSE pump goroutine and its
// request-handling loop can both emit messages without interleaving bytes.
type Server struct {
	r   *Reader
	wmu sync.Mutex
	w   io.Writer
}

// NewServer creates an ACP agent server reading client messages from r and
// writing responses/notifications to w.
func NewServer(r io.Reader, w io.Writer) *Server { return &Server{r: NewReader(r), w: w} }

// Read returns the next message from the client (a request with an ID, or a
// notification with none).
func (s *Server) Read() (Message, error) { return s.r.ReadMessage() }

// write serializes all writes to the underlying writer.
func (s *Server) write(m Message) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return WriteMessage(s.w, m)
}

// Respond writes a successful JSON-RPC response for the given request id.
func (s *Server) Respond(id int, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.write(Message{ID: IntID(id), Result: b})
}

// RespondError writes a JSON-RPC error response for the given request id.
func (s *Server) RespondError(id, code int, message string) error {
	return s.write(Message{ID: IntID(id), Error: &RPCError{Code: code, Message: message}})
}

// Notify writes a notification (no id), e.g. session/update.
func (s *Server) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.write(Message{Method: method, Params: b})
}

// Request writes a server-initiated request with the given id, e.g.
// session/request_permission. The client replies with a response carrying the
// same id, which the caller reads via Read.
func (s *Server) Request(id int, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.write(Message{ID: IntID(id), Method: method, Params: b})
}
