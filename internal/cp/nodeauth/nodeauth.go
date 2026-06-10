// Package nodeauth authenticates nodes connecting to the CP: it derives a node's identity
// (nodeId/accountId/class) from its verified mTLS client certificate, instead of trusting the
// self-asserted Register fields. Class is whatever the name-constrained chain proves — a self-hosted
// authority can't yield a cloud identity (see node-auth design §5; pki enforces it).
package nodeauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"time"

	"spawnery/internal/pki"
)

// Mode selects node-auth enforcement. insecure (dev/test): no client certs, identity falls back to the
// self-asserted Register fields. enforced (staging/prod): mTLS required, identity from the verified cert.
type Mode string

const (
	ModeInsecure Mode = "insecure"
	ModeEnforced Mode = "enforced"
)

// Middleware authenticates node connections before they reach the handler. In enforced mode it derives
// the identity from the client cert (401 if absent/invalid) and stashes it on the request context; in
// insecure mode it passes through with no identity.
func Middleware(mode Mode, root *x509.Certificate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == ModeEnforced {
			id, err := DeriveIdentity(r.TLS, root, time.Now())
			if err != nil {
				http.Error(w, "node authentication required", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(WithIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// DeriveIdentity verifies the TLS peer's client certificate chain against the pinned root and returns
// the node identity from its SAN. It rejects connections with no client certificate.
func DeriveIdentity(state *tls.ConnectionState, root *x509.Certificate, now time.Time) (pki.Identity, error) {
	if state == nil || len(state.PeerCertificates) == 0 {
		return pki.Identity{}, errors.New("nodeauth: no client certificate")
	}
	leaf := state.PeerCertificates[0]
	intermediates := state.PeerCertificates[1:]
	return pki.Verify(leaf, intermediates, root, now)
}

type ctxKey struct{}

// WithIdentity stashes a verified node identity on the context (set by the CP's node-auth middleware,
// read by the Attach handler).
func WithIdentity(ctx context.Context, id pki.Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFromContext returns the verified node identity, if the connection was authenticated.
func IdentityFromContext(ctx context.Context) (pki.Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(pki.Identity)
	return id, ok
}
