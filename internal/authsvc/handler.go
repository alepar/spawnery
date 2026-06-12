package authsvc

import (
	"encoding/base64"
	"net/http"
)

// Handler returns the AS's HTTP surface. The skeleton serves liveness and the Root CA for
// distribution; enrollment (sp-0qc) and session signing (sp-3ca) add their routes here. NOTE: the Root
// CA is the trust anchor and is meant to be pinned OUT-OF-BAND (baked into client bundles); this
// endpoint exists for bootstrap/ops convenience, not as the trust mechanism.
//
// Identity routes (A1, sp-ussy.1) are registered when an *IdP is attached via WithIdP:
//   GET  /oauth/authorize     — start auth-code+PKCE flow (rate-limited)
//   GET  /oauth/callback      — GitHub redirects here; mints tokens
//   POST /refresh             — credentialed-CORS; PoP-gated rotation [AM2,AM5]
//   POST /logout              — revoke family + expire cookie [AM10]
//   GET  /revocations         — signed revocation feed for CP (A2) [AM10]
//   POST /device/authorize    — RFC 8628: spawnctl POSTs session pubkey [AM7]
//   GET  /device/verify       — user confirmation page (authed browser) [AM7]
//   POST /device/verify       — user submits user_code [AM7]
//   POST /device/token        — spawnctl polls for tokens [AM7]
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /ca/root", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(s.RootCAPEM())
	})
	mux.HandleFunc("POST /enroll", s.enrollHandler)
	mux.HandleFunc("GET /session/pubkey", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(base64.RawURLEncoding.EncodeToString(s.SessionPubKey())))
	})

	if s.idp != nil {
		idp := s.idp
		// OAuth flow.
		mux.HandleFunc("GET /oauth/authorize", idp.serveAuthorize)
		mux.HandleFunc("GET /oauth/callback", idp.serveCallback)

		// /refresh is credentialed-CORS [AM2]: wrap with CORS middleware.
		mux.HandleFunc("POST /refresh", idp.corsCredentialed(idp.serveRefresh))
		mux.HandleFunc("OPTIONS /refresh", idp.corsCredentialed(func(w http.ResponseWriter, r *http.Request) {
			// Pre-flight handled by corsCredentialed itself.
		}))

		// Logout.
		mux.HandleFunc("POST /logout", idp.corsCredentialed(idp.serveLogout))
		mux.HandleFunc("OPTIONS /logout", idp.corsCredentialed(func(w http.ResponseWriter, r *http.Request) {
			// Pre-flight handled by corsCredentialed itself.
		}))

		// Revocation feed (A2 consumption) — no CORS needed (server-to-server).
		mux.HandleFunc("GET /revocations", idp.serveRevocations)

		// Device grant (RFC 8628) [AM7].
		mux.HandleFunc("POST /device/authorize", idp.serveDeviceAuthorize)
		mux.HandleFunc("GET /device/verify", idp.serveDeviceVerifyGet)
		mux.HandleFunc("POST /device/verify", idp.serveDeviceVerifyPost)
		mux.HandleFunc("POST /device/token", idp.serveDeviceToken)
	}

	return mux
}
