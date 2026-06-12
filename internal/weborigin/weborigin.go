// Package weborigin implements the browser-origin allowlist shared by the CP and the AS
// (web-epic W1, sp-2ckv.6). It governs both CORS on Connect-JSON RPCs and the WebSocket
// upgrade Origin check (CORS does not cover WS upgrades — [WM18]).
//
// Origin checks are a browser-confinement mechanism: requests WITHOUT an Origin header
// (curl, spawnctl-class tooling, node->CP Connect calls) are always allowed — a non-browser
// client can forge any Origin anyway, so denying header-less requests buys nothing and
// breaks every native client.
package weborigin

import (
	"net/http"
	"net/url"
	"strings"
)

// Allowlist is an exact-match set of allowed browser origins. The zero value allows
// nothing browser-originated; build one with FromEnv.
type Allowlist struct {
	origins map[string]bool
	dev     bool
}

// FromEnv parses a comma-separated list of exact origins (e.g.
// "https://app.spawnery.dev"). An empty value means DEV MODE: any localhost /
// 127.0.0.1 / [::1] origin is allowed. A non-empty list is exact, case-insensitive
// full-origin match ONLY — localhost is never implicit in production ([WL5]).
func FromEnv(val string) Allowlist {
	val = strings.TrimSpace(val)
	if val == "" {
		return Allowlist{dev: true}
	}
	set := map[string]bool{}
	for _, o := range strings.Split(val, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		set[strings.ToLower(strings.TrimSuffix(o, "/"))] = true
	}
	return Allowlist{origins: set}
}

// Dev reports whether the allowlist is in dev mode (no origins configured).
func (a Allowlist) Dev() bool { return a.dev }

// Allowed reports whether the given Origin header value may interact with the service.
// Empty origin (non-browser client) is always allowed; see the package comment.
func (a Allowlist) Allowed(origin string) bool {
	if origin == "" {
		return true
	}
	origin = strings.ToLower(strings.TrimSuffix(origin, "/"))
	if a.dev {
		return isLoopbackOrigin(origin)
	}
	return a.origins[origin]
}

// isLoopbackOrigin matches http(s)://localhost[:port], 127.0.0.1, and [::1] origins.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// CORS wraps next with an allowlist-driven CORS layer for Connect-JSON RPCs.
// Allowed origins get an echoed Access-Control-Allow-Origin (+ Vary: Origin);
// preflights for allowed origins are answered 204 here and never reach next.
// Disallowed origins pass through with NO ACAO headers — the browser blocks the
// response, non-browser clients are unaffected.
func (a Allowlist) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !a.Allowed(origin) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Connect-Protocol-Version, Connect-Timeout-Ms")
			h.Set("Access-Control-Max-Age", "7200")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
