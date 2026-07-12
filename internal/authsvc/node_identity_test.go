package authsvc

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/pki"
)

func TestNodeIdentityMiddlewareRejectsConfiguredTrustDomainMismatch(t *testing.T) {
	root, _ := pki.NewRootCA("root")
	issuer, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "staging.spawnery.internal")
	leaf, _ := issuer.IssueNode("n", "a", pki.RoleSelfHosted, "staging.spawnery.internal", time.Now().Add(time.Hour))
	var seen string
	var ok bool
	h := nodeIdentityMiddleware(root.Cert, nodeIDProbe(&seen, &ok), "prod.spawnery.internal")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Cert, issuer.Cert}}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if ok || seen != "" {
		t.Fatalf("wrong-trust-domain identity reached handler: ok=%v id=%q", ok, seen)
	}
}

// probe handler records whether a node identity was present in context.
func nodeIDProbe(seen *string, ok *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, present := nodeIDFromContext(r.Context())
		*seen, *ok = id, present
		w.WriteHeader(http.StatusOK)
	})
}

func TestDevNodeIdentityMiddleware_FillsFromHeaderWhenNoTLS(t *testing.T) {
	var seen string
	var ok bool
	h := devNodeIdentityMiddleware("X-Spawnery-Dev-Node-Id", nodeIDProbe(&seen, &ok))
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Spawnery-Dev-Node-Id", "node-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ok || seen != "node-1" {
		t.Fatalf("want node-1 identity from header, got ok=%v id=%q", ok, seen)
	}
}

func TestDevNodeIdentityMiddleware_NoHeaderNoIdentity(t *testing.T) {
	var seen string
	var ok bool
	h := devNodeIdentityMiddleware("X-Spawnery-Dev-Node-Id", nodeIDProbe(&seen, &ok))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	if ok || seen != "" {
		t.Fatalf("want no identity without header, got ok=%v id=%q", ok, seen)
	}
}

func TestDevNodeIdentityMiddleware_DoesNotOverrideExistingIdentity(t *testing.T) {
	var seen string
	var ok bool
	// Outer mw pre-seeds a "real" (TLS-equivalent) identity; dev mw (inner) must NOT clobber it.
	inner := devNodeIdentityMiddleware("X-Spawnery-Dev-Node-Id", nodeIDProbe(&seen, &ok))
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withNodeIdentity(r.Context(), pki.Principal{Kind: pki.KindNode, NodeID: "real-node"}))
		inner.ServeHTTP(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Spawnery-Dev-Node-Id", "spoofed")
	outer.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "real-node" {
		t.Fatalf("dev header must not override verified identity, got %q", seen)
	}
}

func TestHandler_IgnoresDevHeaderWhenOptionUnset(t *testing.T) {
	// When devNodeIdentityHeader is unset (default), the dev header must be completely inert.
	// Test this at the middleware-composition level: no dev wrap → header has no effect.
	var seen string
	var ok bool
	// nodeIdentityMiddleware with nil root: only acts on r.TLS peer certs (nil → no identity set).
	h := nodeIdentityMiddleware(nil, nodeIDProbe(&seen, &ok))
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Spawnery-Dev-Node-Id", "hacker-node")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if ok || seen != "" {
		t.Fatalf("nodeIdentityMiddleware without devNodeIdentityMiddleware must ignore header, got ok=%v id=%q", ok, seen)
	}
}
