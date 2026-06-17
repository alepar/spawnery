package authsvc

import "net/http"

// ghLinkCORSCredentialed wraps POST /github/link/redeem: it carries the HttpOnly SameSite=Strict
// completer cookie cross-origin, so the response MUST send ACAC:true + EXACT-origin ACAO
// (corsBearerSimple omits ACAC and would silently break web redeem -- spike S1). Foreign Origin
// -> 403; Origin-less CLI passes through. Authorization allowed (redeem is Bearer-authenticated
// too).
func (s *Service) ghLinkCORSCredentialed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if s.githubLinkSPAOrigin == "" || origin != s.githubLinkSPAOrigin {
				writeError(w, http.StatusForbidden, "origin_forbidden", "origin not allowed on credentialed endpoint")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ghLinkCORSBearerSimple wraps the Bearer-only link endpoints (start, GET /github/links, revoke):
// origin-checked, no Allow-Credentials. Mirrors deviceset corsBearerSimple.
func (s *Service) ghLinkCORSBearerSimple(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if s.githubLinkSPAOrigin == "" || origin != s.githubLinkSPAOrigin {
				writeError(w, http.StatusForbidden, "origin_forbidden", "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
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
