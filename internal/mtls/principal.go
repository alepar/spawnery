package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"

	"spawnery/internal/pki"
)

type principalContextKey struct{}

// PrincipalMiddleware verifies a completed internal TLS connection's optional client certificate
// and attaches its typed principal to the request context. A completed connection without a client
// certificate remains anonymous for route policy to decide.
func PrincipalMiddleware(verifier *PeerVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if verifier == nil || r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if len(r.TLS.PeerCertificates) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := verifier.verifyPresented(r.TLS.PeerCertificates, x509.ExtKeyUsageClientAuth)
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
