package mtls

import (
	"context"
	"crypto/x509"
	"net/http"
	"time"

	"spawnery/internal/pki"
)

type principalContextKey struct{}

// PrincipalMiddleware verifies a completed internal TLS connection's optional client certificate
// and attaches its typed principal to the request context. A completed connection without a client
// certificate remains anonymous for route policy to decide.
func PrincipalMiddleware(root *x509.Certificate, trustDomain string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !r.TLS.HandshakeComplete {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(r.TLS.PeerCertificates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := verifyPresentedPrincipal(r.TLS.PeerCertificates, root, trustDomain, time.Now, nil, x509.ExtKeyUsageClientAuth)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PrincipalFromContext returns the verified Spawnery principal attached to ctx.
func PrincipalFromContext(ctx context.Context) (pki.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(pki.Principal)
	return principal, ok
}
