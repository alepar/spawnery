package spawnlet

import (
	"encoding/json"
	"net/http"

	"github.com/coder/websocket"
)

// HandleWS bridges a browser WebSocket to a spawn's agent stdio via the
// transparent Relay. First message: {"spawnId":"..."} (text); then raw ACP bytes
// in both directions. Same byte relay as the ConnectRPC Session, different transport.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // dev only; tighten when CP/auth lands
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(16 * 1024 * 1024)
	ctx := r.Context()
	defer func() { _ = conn.CloseNow() }()

	_, first, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var bind struct {
		SpawnID string `json:"spawnId"`
	}
	if err := json.Unmarshal(first, &bind); err != nil {
		conn.Close(websocket.StatusUnsupportedData, "bad bind frame")
		return
	}
	sp, ok := s.m.Store().Get(bind.SpawnID)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "unknown spawn")
		return
	}
	att, err := s.m.Attach(ctx, sp)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer func() { _ = att.Close() }()

	ep := StreamEndpoint{
		Recv: func() ([]byte, error) {
			_, b, err := conn.Read(ctx)
			return b, err
		},
		Send: func(b []byte) error {
			return conn.Write(ctx, websocket.MessageBinary, b)
		},
	}
	Relay(ctx, ep, AgentIO{Stdin: att.Stdin, Stdout: att.Stdout})
	conn.Close(websocket.StatusNormalClosure, "")
}
