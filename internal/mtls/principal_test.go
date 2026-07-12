package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"spawnery/internal/pki"
)

func TestPrincipalMiddlewarePropagatesVerifiedPrincipal(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	want := pki.Principal{
		TrustDomain: testTrustDomain,
		Kind:        pki.KindNode,
		Role:        pki.RoleSelfHosted,
		AccountID:   "acct-1",
		NodeID:      "node-1",
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := PrincipalFromContext(r.Context())
		if !ok {
			t.Fatal("verified principal missing from context")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("principal = %+v, want %+v", got, want)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := requestWithPeer("/node", f.selfHosted)
	rec := httptest.NewRecorder()

	PrincipalMiddleware(f.root.Cert, testTrustDomain, next).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPrincipalMiddlewareLeavesAbsentPeerAnonymous(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := PrincipalFromContext(r.Context()); ok {
			t.Fatalf("anonymous request has principal %+v", principal)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/enroll", nil)
	req.TLS = &tls.ConnectionState{HandshakeComplete: true}
	rec := httptest.NewRecorder()

	PrincipalMiddleware(f.root.Cert, testTrustDomain, next).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPrincipalMiddlewareRejectsUnverifiedOrInvalidTLSState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  func(*testing.T, *tlsFixture) *http.Request
	}{
		{
			name: "cleartext request",
			req: func(_ *testing.T, _ *tlsFixture) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/enroll", nil)
			},
		},
		{
			name: "handshake incomplete",
			req: func(_ *testing.T, f *tlsFixture) *http.Request {
				req := requestWithPeer("/node", f.selfHosted)
				req.TLS.HandshakeComplete = false
				return req
			},
		},
		{
			name: "wrong root",
			req: func(t *testing.T, _ *tlsFixture) *http.Request {
				return requestWithPeer("/node", newTLSFixture(t).selfHosted)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newTLSFixture(t)
			reached := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
			rec := httptest.NewRecorder()

			PrincipalMiddleware(f.root.Cert, testTrustDomain, next).ServeHTTP(rec, tt.req(t, f))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if reached {
				t.Fatal("invalid TLS state reached next handler")
			}
		})
	}
}

func requestWithPeer(path string, identity *pki.Leaf) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  append([]*x509.Certificate{identity.Cert}, identity.Chain...),
	}
	return req
}
