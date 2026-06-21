package cp

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/telemetry"
	slogctx "spawnery/internal/log"
	"spawnery/internal/weborigin"
)

// HandleWS bridges a browser WebSocket to a spawn via the router. First message:
// {"spawnId":"...","token":"..."} (text); then raw ACP bytes (BINARY) both ways.
// In-band reauth: a TEXT frame {"type":"reauth","token":"<wire>"} resets the deadline
// and re-registers the session under the new token_id. An invalid reauth closes the
// connection with StatusPolicyViolation in prod; dev mode logs but stays open.
//
// CORS does not govern WS upgrades, so the Origin header is validated here against the
// same allowlist as the Connect RPCs ([WM18]). Auth stays the in-band token bind frame —
// browsers cannot attach headers to a WebSocket.
func (s *Server) HandleWS(v *auth.Verifier, allow weborigin.Allowlist) http.HandlerFunc {
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
			SpawnID      string `json:"spawnId"`
			Token        string `json:"token"`
			ClientID     string `json:"clientId"`
			SessionID    string `json:"sessionId"` // empty => session #0
			Cursor       int64  `json:"cursor"`
			// signedIntent (A4): raw proto.Marshal(SignedIntent) bytes, base64-encoded [AC1][AM12].
			// If present the CP mints a dev aud=node token and threads the envelope into SessionOpen.
			SignedIntent []byte `json:"signedIntent,omitempty"`
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

		// Per-session context for revocation cancellation; enriched with correlation IDs so all
		// logging within the session includes spawn_id and session_id automatically.
		sessCtx, sessCancel := context.WithCancel(ctx)
		defer sessCancel()
		sessCtx = slogctx.WithSpawnID(sessCtx, bind.SpawnID)
		sessCtx = slogctx.WithSessionID(sessCtx, sessionID)

		// Track current session registration; swapped on token rotation to release the old id promptly.
		var (
			curRelMu   sync.Mutex
			curRelease func()
		)
		if s.sessions != nil {
			curRelease = s.sessions.Add(identity.TokenID, identity.Owner, sessCancel)
		}
		defer func() {
			curRelMu.Lock()
			defer curRelMu.Unlock()
			if curRelease != nil {
				curRelease()
			}
		}()

		// A4: build session-open AuthEnvelope from the client-supplied SignedIntent bytes [AC1][AM12].
		var sessionEnv *authv1.AuthEnvelope
		if len(bind.SignedIntent) > 0 {
			var si authv1.SignedIntent
			if merr := proto.Unmarshal(bind.SignedIntent, &si); merr == nil {
				sessionEnv = s.mintSessionEnv(owner, &authv1.AuthEnvelope{Intent: &si})
			} else {
				slogctx.FromContext(sessCtx).Warn("ws: bind frame signedIntent unmarshal failed", "err", merr)
			}
		}
		cs := wsClient{conn: conn, ctx: sessCtx}
		done, err := s.rt.AttachClient(bind.SpawnID, sessionID, bind.ClientID, owner, sessionEnv, cs, bind.Cursor)
		if err != nil {
			slogctx.FromContext(sessCtx).Error("ws: session attach failed", "err", err)
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
							slogctx.FromContext(sessCtx).Warn("ws reauth failed (dev-tolerant)", "err", verr)
						} else {
							// Re-register under new token_id; release old so only one id is active.
							if s.sessions != nil && newID.TokenID != "" && newID.TokenID != identity.TokenID {
								newRel := s.sessions.Add(newID.TokenID, newID.Owner, sessCancel)
								curRelMu.Lock()
								if curRelease != nil {
									curRelease()
								}
								curRelease = newRel
								curRelMu.Unlock()
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
