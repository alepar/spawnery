package cp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/telemetry"
)

// HandleWS bridges a browser WebSocket to a spawn via the router. First message:
// {"spawnId":"...","token":"..."} (text); then raw ACP bytes both ways.
func (s *Server) HandleWS(authn *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}}) // dev only
		if err != nil {
			return
		}
		conn.SetReadLimit(16 * 1024 * 1024)
		ctx := r.Context()
		defer conn.CloseNow()

		_, first, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var bind struct {
			SpawnID  string `json:"spawnId"`
			Token    string `json:"token"`
			ClientID string `json:"clientId"`
			Cursor   int64  `json:"cursor"`
		}
		if err := json.Unmarshal(first, &bind); err != nil {
			conn.Close(websocket.StatusUnsupportedData, "bad bind frame")
			return
		}
		owner, ok := authn.Owner(bind.Token)
		if !ok {
			conn.Close(websocket.StatusPolicyViolation, "unauthenticated")
			return
		}
		sp, err := s.st.Spawns().Get(ctx, bind.SpawnID)
		if err != nil || sp.OwnerID != owner {
			conn.Close(websocket.StatusPolicyViolation, "unknown or foreign spawn")
			return
		}

		if bind.ClientID == "" {
			conn.Close(websocket.StatusUnsupportedData, "clientId required")
			return
		}
		cs := wsClient{conn: conn, ctx: ctx}
		done, err := s.rt.AttachClient(bind.SpawnID, bind.ClientID, cs, bind.Cursor)
		if err != nil {
			conn.Close(websocket.StatusInternalError, "attach failed")
			return
		}
		_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: owner, SpawnID: bind.SpawnID, Timestamp: time.Now().UTC()})
		defer func() {
			s.rt.DetachClient(bind.SpawnID, bind.ClientID)
			_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: bind.SpawnID, Timestamp: time.Now().UTC()})
		}()

		recvErr := make(chan struct{}, 1)
		go func() {
			for {
				_, b, err := conn.Read(ctx)
				if err != nil {
					recvErr <- struct{}{}
					return
				}
				if ferr := s.rt.FromClient(bind.SpawnID, bind.ClientID, b); ferr != nil {
					recvErr <- struct{}{}
					return
				}
			}
		}()
		select {
		case <-done:
		case <-recvErr:
		case <-ctx.Done():
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

type wsClient struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (c wsClient) Send(b []byte) error { return c.conn.Write(c.ctx, websocket.MessageBinary, b) }
