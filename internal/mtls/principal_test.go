package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"math/big"
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
		peer, ok := VerifiedPeerFromContext(r.Context())
		if !ok || peer.IssuerSerial.Cmp(f.selfHostCA.Cert.SerialNumber) != 0 || peer.LeafSerial.Cmp(f.selfHosted.Cert.SerialNumber) != 0 {
			t.Fatalf("verified peer = %+v, present=%v", peer, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := requestWithPeer("/node", f.selfHosted)
	rec := httptest.NewRecorder()

	PrincipalMiddleware(newPeerVerifier(t, f, nil), next).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPrincipalMiddlewareUsesCanonicalVerifiedIssuerWithReorderedExtras(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		peer, ok := VerifiedPeerFromContext(r.Context())
		if !ok || peer.IssuerSerial.Cmp(f.selfHostCA.Cert.SerialNumber) != 0 {
			t.Fatalf("verified issuer = %+v, present=%v", peer, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := requestWithPeer("/node", f.selfHosted)
	req.TLS.PeerCertificates = []*x509.Certificate{f.selfHosted.Cert, f.cloudCA.Cert, f.selfHostCA.Cert}
	rec := httptest.NewRecorder()
	PrincipalMiddleware(newPeerVerifier(t, f, nil), next).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !reached {
		t.Fatalf("status=%d reached=%v", rec.Code, reached)
	}

	missing := requestWithPeer("/node", f.selfHosted)
	missing.TLS.PeerCertificates = []*x509.Certificate{f.selfHosted.Cert, f.cloudCA.Cert}
	rec = httptest.NewRecorder()
	reached = false
	PrincipalMiddleware(newPeerVerifier(t, f, nil), next).ServeHTTP(rec, missing)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("missing issuer status=%d reached=%v", rec.Code, reached)
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
	req.TLS = &tls.ConnectionState{HandshakeComplete: true, Version: tls.VersionTLS13}
	rec := httptest.NewRecorder()

	PrincipalMiddleware(newPeerVerifier(t, f, nil), next).ServeHTTP(rec, req)
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

			PrincipalMiddleware(newPeerVerifier(t, f, nil), next).ServeHTTP(rec, tt.req(t, f))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if reached {
				t.Fatal("invalid TLS state reached next handler")
			}
		})
	}
}

func TestPrincipalMiddlewareRevalidatesRealServerConnectionState(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	revoked := false
	verifier := newPeerVerifier(t, f, func(_, serial *big.Int) bool {
		return revoked && serial.Cmp(f.selfHosted.Cert.SerialNumber) == 0
	})
	serverConfig, err := ServerConfig(ServerOptions{
		Verifier:   verifier,
		Identity:   tlsCertificate(t, f.cp),
		ClientMode: RequireClientCertificate,
	})
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	state, clientErr, serverErr := handshakeServerState(internalClientConfig(t, f, f.selfHosted), serverConfig)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("handshake errors: client=%v server=%v", clientErr, serverErr)
	}

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.Role != pki.RoleSelfHosted || principal.NodeID != "node-1" {
			t.Fatalf("principal = %+v, present=%v", principal, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := PrincipalMiddleware(verifier, next)
	request := httptest.NewRequest(http.MethodPost, "/node", nil)
	request.TLS = &state
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusNoContent || !reached {
		t.Fatalf("verified state: status=%d reached=%v", rec.Code, reached)
	}

	revoked = true
	reached = false
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("revoked state: status=%d reached=%v", rec.Code, reached)
	}
}

func TestPrincipalMiddlewareRejectsNilVerifier(t *testing.T) {
	t.Parallel()
	f := newTLSFixture(t)
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	rec := httptest.NewRecorder()

	PrincipalMiddleware(nil, next).ServeHTTP(rec, requestWithPeer("/node", f.selfHosted))
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("status=%d reached=%v", rec.Code, reached)
	}
}

func requestWithPeer(path string, identity *pki.Leaf) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		Version:           tls.VersionTLS13,
		PeerCertificates:  append([]*x509.Certificate{identity.Cert}, identity.Chain...),
	}
	return req
}
