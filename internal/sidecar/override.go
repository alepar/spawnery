package sidecar

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// Credentials is the immutable upstream/key pair used for one proxied request.
type Credentials struct {
	Upstream string
	Key      string
}

// Override holds the live model override shared by the sidecar's proxy handlers.
// An empty/nil value means passthrough: forward whatever model the agent chose.
// All methods are safe for concurrent use and lock-free on the read path.
type Override struct {
	v     atomic.Pointer[string]
	creds atomic.Pointer[Credentials]
}

// Get returns the current override, or "" for passthrough. Safe on a nil receiver.
func (o *Override) Get() string {
	if o == nil {
		return ""
	}
	if p := o.v.Load(); p != nil {
		return *p
	}
	return ""
}

// Set stores model as the live override. An empty string clears it (passthrough).
func (o *Override) Set(model string) {
	if model == "" {
		o.v.Store(nil)
		return
	}
	o.v.Store(&model)
}

// SetCredentials stores the live upstream/key override. key is required. upstream
// is optional; when empty, handlers keep using their configured default upstream.
func (o *Override) SetCredentials(upstream, key string) error {
	if o == nil {
		return fmt.Errorf("override is nil")
	}
	key = strings.TrimSpace(key)
	upstream = strings.TrimSpace(upstream)
	if key == "" {
		return fmt.Errorf("key is required")
	}
	if upstream != "" {
		u, err := url.Parse(upstream)
		if err != nil {
			return fmt.Errorf("invalid upstream: %w", err)
		}
		if !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("upstream must be an absolute http(s) URL")
		}
	}
	o.creds.Store(&Credentials{Upstream: upstream, Key: key})
	return nil
}

// Credentials returns the live credential override, falling back to the handler's
// configured defaults. Safe on a nil receiver.
func (o *Override) Credentials(defaultUpstream, defaultKey string) Credentials {
	if o == nil {
		return Credentials{Upstream: defaultUpstream, Key: defaultKey}
	}
	if c := o.creds.Load(); c != nil {
		out := *c
		if out.Upstream == "" {
			out.Upstream = defaultUpstream
		}
		return out
	}
	return Credentials{Upstream: defaultUpstream, Key: defaultKey}
}

// patchModelJSON returns body with its top-level "model" field replaced by model,
// preserving all other fields. It errors if body is not a JSON object.
func patchModelJSON(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		// body was a JSON null (or otherwise not an object); a nil map cannot be
		// assigned to. Treat as non-object so the caller forwards the original bytes.
		return nil, fmt.Errorf("body is not a JSON object")
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = mb
	return json.Marshal(obj)
}

// NewControlHandler serves the token-gated control endpoints:
//
//	/control/model  POST {"model":"<openrouter-id>"} -> set the live override (empty clears it)
//	/control/model  GET                              -> {"model":"<current override>"}
//	/control/credentials POST {"upstream":"<url>","key":"<api-key>"} -> set live credentials
//	/control/status GET                              -> {"model":"<current override>","busy":bool,"active_requests":n}
//
// Auth: Authorization: Bearer <token>, constant-time compared against token.
// Intended to run on its own http.Server bound to the pod IP (not loopback).
func NewControlHandler(ov *Override, token string, trackers ...*Inflight) http.Handler {
	inflight := firstInflight(trackers)
	mux := http.NewServeMux()
	mux.HandleFunc("/control/model", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"model": ov.Get()})
		case http.MethodPost:
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
			ov.Set(body.Model)
			writeJSON(w, http.StatusOK, map[string]any{"model": ov.Get(), "applied": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/control/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Key      string `json:"key"`
			Upstream string `json:"upstream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := ov.SetCredentials(body.Upstream, body.Key); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"applied": true, "upstream_set": strings.TrimSpace(body.Upstream) != ""})
	})
	mux.HandleFunc("/control/status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		active := inflight.Active()
		writeJSON(w, http.StatusOK, map[string]any{"model": ov.Get(), "busy": active > 0, "active_requests": active})
	})
	return mux
}

// authorized reports whether r carries a valid "Bearer <token>" header.
// An empty configured token denies all (the control server should not be started without one).
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("sidecar: control writeJSON failed", "err", err)
	}
}
