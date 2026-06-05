package nodeauth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/pki"
)

// In enforced mode the middleware derives the identity from the peer cert and hands it to the next
// handler via context.
func TestMiddlewareEnforcedSetsIdentity(t *testing.T) {
	root, state := issue(t, pki.ClassSelfHosted)
	var got pki.Identity
	var ok bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = nodeauth.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.TLS = state
	rec := httptest.NewRecorder()
	nodeauth.Middleware(nodeauth.ModeEnforced, root, next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !ok || got.NodeID != "n1" {
		t.Fatalf("code=%d ok=%v id=%+v", rec.Code, ok, got)
	}
}

// In enforced mode a connection with no client cert is rejected (401) and never reaches the handler.
func TestMiddlewareEnforcedRejectsNoCert(t *testing.T) {
	root, _ := issue(t, pki.ClassSelfHosted)
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest(http.MethodPost, "/", nil) // no TLS
	rec := httptest.NewRecorder()
	nodeauth.Middleware(nodeauth.ModeEnforced, root, next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("code=%d reached=%v, want 401 + not reached", rec.Code, reached)
	}
}

// In insecure mode the middleware passes through with no identity (dev/test).
func TestMiddlewareInsecurePassesThrough(t *testing.T) {
	hadID := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadID = nodeauth.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	nodeauth.Middleware(nodeauth.ModeInsecure, nil, next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || hadID {
		t.Fatalf("code=%d hadID=%v, want 200 + no identity", rec.Code, hadID)
	}
}

func issue(t *testing.T, class string) (root *x509.Certificate, state *tls.ConnectionState) {
	t.Helper()
	r, _ := pki.NewRootCA("R")
	inter, _ := r.NewIntermediate(class)
	node, _ := inter.IssueNode("n1", "acct1", class, time.Now().Add(time.Hour))
	return r.Cert, &tls.ConnectionState{PeerCertificates: []*x509.Certificate{node.Cert, inter.Cert}}
}

// A verified mTLS peer cert yields the node identity (from the SAN), derived against the pinned root.
func TestDeriveIdentity(t *testing.T) {
	root, state := issue(t, pki.ClassSelfHosted)
	id, err := nodeauth.DeriveIdentity(state, root, time.Now())
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	if id.NodeID != "n1" || id.AccountID != "acct1" || id.Class != pki.ClassSelfHosted {
		t.Fatalf("identity = %+v", id)
	}
}

// No client cert presented -> rejected.
func TestDeriveIdentityNoCert(t *testing.T) {
	root, _ := issue(t, pki.ClassSelfHosted)
	if _, err := nodeauth.DeriveIdentity(&tls.ConnectionState{}, root, time.Now()); err == nil {
		t.Fatal("a connection with no peer cert must be rejected")
	}
	if _, err := nodeauth.DeriveIdentity(nil, root, time.Now()); err == nil {
		t.Fatal("a nil TLS state must be rejected")
	}
}

// A cloud-SAN leaf forged by a self-hosted intermediate must NOT yield an identity (name constraints).
func TestDeriveIdentityRejectsForgedCloud(t *testing.T) {
	root, _ := pki.NewRootCA("R")
	selfHosted, _ := root.NewIntermediate(pki.ClassSelfHosted)
	forged, _ := selfHosted.IssueNode("evil", "victim", pki.ClassCloud, time.Now().Add(time.Hour))
	state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{forged.Cert, selfHosted.Cert}}
	if _, err := nodeauth.DeriveIdentity(state, root.Cert, time.Now()); err == nil {
		t.Fatal("SECURITY: a forged cloud leaf must not derive an identity")
	}
}

// Identity round-trips through the request context (how the middleware hands it to the Attach handler).
func TestIdentityContext(t *testing.T) {
	want := pki.Identity{NodeID: "n", AccountID: "a", Class: pki.ClassCloud}
	ctx := nodeauth.WithIdentity(context.Background(), want)
	got, ok := nodeauth.IdentityFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v, want %+v", got, ok, want)
	}
	if _, ok := nodeauth.IdentityFromContext(context.Background()); ok {
		t.Fatal("empty context must report no identity")
	}
}
