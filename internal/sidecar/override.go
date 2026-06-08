package sidecar

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
)

// Override holds the live model override shared by the sidecar's proxy handlers.
// An empty/nil value means passthrough: forward whatever model the agent chose.
// All methods are safe for concurrent use and lock-free on the read path.
type Override struct {
	v atomic.Pointer[string]
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

// NewControlHandler serves the token-gated /control/model endpoint:
//
//	POST {"model":"<openrouter-id>"}  -> set the live override (empty clears it)
//	GET                               -> {"model":"<current override>"}
//
// Auth: Authorization: Bearer <token>, constant-time compared against token.
// Intended to run on its own http.Server bound to the pod IP (not loopback).
func NewControlHandler(ov *Override, token string) http.Handler {
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
		log.Printf("warn: sidecar: control writeJSON: %v", err)
	}
}
