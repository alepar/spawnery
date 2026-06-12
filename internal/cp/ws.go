package cp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/weborigin"
)

// HandleWS bridges a browser WebSocket to a spawn via the router. First message:
// {"spawnId":"...","token":"..."} (text); then raw ACP bytes both ways.
//
// CORS does not govern WS upgrades, so the Origin header is validated here against the
// same allowlist as the Connect RPCs ([WM18]). Auth stays the in-band token bind frame —
// browsers cannot attach headers to a WebSocket.
func (s *Server) HandleWS(authn *auth.Auth, allow weborigin.Allowlist) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allow.Allowed(r.Header.Get("Origin")) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		// Our Origin check already ran (scheme-exact, unlike OriginPatterns' host-only match),
		// so skip coder/websocket's own verification.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
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
			SpawnID   string `json:"spawnId"`
			Token     string `json:"token"`
			ClientID  string `json:"clientId"`
			SessionID string `json:"sessionId"` // empty => session #0
			Cursor    int64  `json:"cursor"`
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
		sessionID := bind.SessionID
		if sessionID == "" {
			sessionID = "0" // default to session #0 (backward compat with single-session clients)
		}
		cs := wsClient{conn: conn, ctx: ctx}
		done, err := s.rt.AttachClient(bind.SpawnID, sessionID, bind.ClientID, cs, bind.Cursor)
		if err != nil {
			conn.Close(websocket.StatusInternalError, "attach failed")
			return
		}
		_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: owner, SpawnID: bind.SpawnID, Timestamp: time.Now().UTC()})
		defer func() {
			s.rt.DetachClient(bind.SpawnID, sessionID, bind.ClientID)
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
				if ferr := s.rt.FromClient(bind.SpawnID, sessionID, bind.ClientID, b); ferr != nil {
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
