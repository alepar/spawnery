package authsvc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

// Full enrollment vertical: the node-side client generates a keypair+CSR, POSTs the token+CSR to the AS
// /enroll endpoint, and gets back a self-hosted cert+chain bound to the token's account and the node's
// own key — verifiable against the AS root.
func TestEnrollHTTPRoundTrip(t *testing.T) {
	s := newAS(t)
	tok, _ := s.IssueEnrollmentToken("acct-Z")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := authsvc.RunEnroll(context.Background(), srv.URL, tok, "node-q")
	if err != nil {
		t.Fatalf("RunEnroll: %v", err)
	}
	cert, err := pki.ParseCertPEM(res.CertPEM)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	inter, _ := pki.ParseCertPEM(res.ChainPEM)
	root, _ := pki.ParseCertPEM(s.RootCAPEM())
	id, err := pki.Verify(cert, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.NodeID != "node-q" || id.AccountID != "acct-Z" || id.Class != pki.ClassSelfHosted {
		t.Fatalf("identity = %+v", id)
	}
	key, err := pki.ParseKeyPEM(res.KeyPEM)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(key.Public()) {
		t.Fatal("returned key does not match the issued cert")
	}
}

func TestEnrollHTTPRejectsBadToken(t *testing.T) {
	s := newAS(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if _, err := authsvc.RunEnroll(context.Background(), srv.URL, "bad-token", "n"); err == nil {
		t.Fatal("a bad token must fail enrollment over HTTP")
	}
}
