package cp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/telemetry"
)

// HandleWS bridges a browser WebSocket to a spawn via the router. First message:
// {"spawnId":"...","token":"..."} (text); then raw ACP bytes (BINARY) both ways.
// In-band reauth: a TEXT frame {"type":"reauth","token":"<wire>"} resets the deadline
// and re-registers the session under the new token_id. An invalid reauth closes the
// connection with StatusPolicyViolation in prod; dev mode logs but stays open.
func (s *Server) HandleWS(v *auth.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}}) // WM18 is web-epic W1's concern
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

		// Verify the bind token through the seam (AS or dev fallback).
		identity, err := v.Verify(bind.Token)
		if err != nil {
			conn.Close(websocket.StatusPolicyViolation, "unauthenticated")
			return
		}
		owner := identity.Owner

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

		// Per-session context for revocation cancellation.
		sessCtx, sessCancel := context.WithCancel(ctx)
		defer sessCancel()

		// Register for revocation fan-out.
		if s.sessions != nil {
			release := s.sessions.Add(identity.TokenID, identity.Owner, sessCancel)
			defer release()
		}

		cs := wsClient{conn: conn, ctx: sessCtx}
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

		// Reauth deadline: AS-token sessions in prod close if no reauth received in time.
		// Dev-token sessions (TokenID=="") and dev mode: timer only logs.
		reauthInterval := s.reauthInterval
		if reauthInterval <= 0 {
			reauthInterval = defaultReauthInterval
		}
		isProdAS := identity.TokenID != "" && !s.devMode
		var reauthTimer *time.Timer
		if isProdAS {
			reauthTimer = time.NewTimer(reauthInterval + reauthGrace)
		}
		var reauthCh <-chan time.Time
		if reauthTimer != nil {
			reauthCh = reauthTimer.C
			defer reauthTimer.Stop()
		}

		recvErr := make(chan struct{}, 1)
		go func() {
			for {
				msgType, b, rerr := conn.Read(sessCtx)
				if rerr != nil {
					recvErr <- struct{}{}
					return
				}
				// TEXT frames are control (reauth); BINARY frames are ACP data.
				if msgType == websocket.MessageText {
					var ctrl struct {
						Type  string `json:"type"`
						Token string `json:"token"`
					}
					if jErr := json.Unmarshal(b, &ctrl); jErr != nil || ctrl.Type != "reauth" {
						// Unknown control — ignore.
						continue
					}
					if s.verify != nil {
						newID, verr := s.verify(ctrl.Token)
						if verr != nil || newID.Owner != owner {
							if !s.devMode {
								conn.Close(websocket.StatusPolicyViolation, "reauth failed")
								recvErr <- struct{}{}
								return
							}
							log.Printf("ws reauth failed (dev-tolerant): %v", verr)
						} else {
							// Re-register under new token_id.
							if s.sessions != nil && newID.TokenID != "" && newID.TokenID != identity.TokenID {
								rel := s.sessions.Add(newID.TokenID, newID.Owner, sessCancel)
								defer rel()
							}
							identity = newID
							// Reset the deadline.
							if reauthTimer != nil {
								if !reauthTimer.Stop() {
									select {
									case <-reauthTimer.C:
									default:
									}
								}
								reauthTimer.Reset(reauthInterval + reauthGrace)
							}
						}
					}
					continue
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
		case <-sessCtx.Done():
		case <-ctx.Done():
		case <-reauthCh:
			conn.Close(websocket.StatusPolicyViolation, "reauth timeout")
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

type wsClient struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (c wsClient) Send(b []byte) error { return c.conn.Write(c.ctx, websocket.MessageBinary, b) }
