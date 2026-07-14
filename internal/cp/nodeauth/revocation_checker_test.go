package nodeauth_test

import (
	"crypto/x509"
	"math/big"
	"net/http"
	"testing"

	"spawnery/internal/cp/nodeauth"
)

func allowNoCertificateRevocations(*big.Int, *big.Int) bool { return false }

func mustMiddleware(t *testing.T, mode nodeauth.Mode, root *x509.Certificate, next http.Handler, trustDomains ...string) http.Handler {
	t.Helper()
	handler, err := nodeauth.Middleware(mode, root, allowNoCertificateRevocations, next, trustDomains...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
