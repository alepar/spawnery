package authsvc

import "net/http"

// corsCredentialed wraps the AS's credentialed endpoints (/refresh, /logout, device endpoints)
// with the [AM2] contract: exact-origin Access-Control-Allow-Origin for THE
// canonical SPA origin + Allow-Credentials, Vary: Origin, and a hard 403 for any other Origin —
// a foreign origin is rejected, not merely left header-less, because these endpoints carry the
// refresh cookie. Requests without an Origin header (CLI, curl) pass through untouched.
//
// Deployment mandate (not enforceable in code): the AS and SPA share one registrable domain
// not on the PSL (auth.X next to app.X) — cross-SITE placement breaks silent refresh under
// Safari ITP / Firefox TCP. See deploy/authsvc/README.md.
func (i *IdP) corsCredentialed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if i.cfg.SPAOrigin == "" || origin != i.cfg.SPAOrigin {
				writeError(w, http.StatusForbidden, "origin_forbidden", "origin not allowed on credentialed endpoint")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
