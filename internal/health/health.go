// Package health registers /healthz and /readyz handlers on an http.ServeMux.
// Both endpoints bypass auth (registered directly on the mux, not behind any interceptor).
package health

import (
	"context"
	"fmt"
	"net/http"
)

// Register adds GET /healthz (static 200 "ok") and GET /readyz (calls ready if non-nil;
// returns 200 "ok" on nil error, 503 "not ready: <err>" on non-nil) to mux.
// If ready is nil, /readyz always returns 200.
func Register(mux *http.ServeMux, ready func(context.Context) error) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if err := ready(r.Context()); err != nil {
			http.Error(w, fmt.Sprintf("not ready: %v", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}
