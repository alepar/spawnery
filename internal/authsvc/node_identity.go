package authsvc

import (
	"context"
	"crypto/x509"
	"net/http"
	"strings"
	"time"

	"spawnery/internal/pki"
)

type nodeIdentityContextKey struct{}

func nodeIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(nodeIdentityContextKey{}).(pki.Identity)
	return id.NodeID, ok && id.NodeID != ""
}

func withNodeIdentity(ctx context.Context, id pki.Identity) context.Context {
	return context.WithValue(ctx, nodeIdentityContextKey{}, id)
}

// devNodeIdentityMiddleware is the D3 dev-lane relaxation: when no mTLS-verified node identity is
// already in context, it trusts `header` as the node id. DEV-ONLY — gated by
// WithDevNodeIdentityHeader, which the prod path never sets (containment invariant d).
func devNodeIdentityMiddleware(header string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := nodeIDFromContext(r.Context()); !ok {
			if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
				r = r.WithContext(withNodeIdentity(r.Context(), pki.Identity{NodeID: id}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func nodeIdentityMiddleware(root *x509.Certificate, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if root != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			leaf := r.TLS.PeerCertificates[0]
			intermediates := r.TLS.PeerCertificates[1:]
			if id, err := pki.Verify(leaf, intermediates, root, time.Now()); err == nil {
				r = r.WithContext(withNodeIdentity(r.Context(), id))
			}
		}
		next.ServeHTTP(w, r)
	})
}
