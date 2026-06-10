package authsvc_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newAS(t *testing.T, opts ...authsvc.Option) *authsvc.Service {
	t.Helper()
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	return authsvc.New(root.Cert, inter, opts...)
}

// An enrollment token issued for account A, when redeemed with a node CSR, yields a self-hosted cert
// bound to account A + the requested nodeId + the node's own key — verifiable against the root.
func TestEnrollIssuesAccountBoundCert(t *testing.T) {
	s := newAS(t)
	tok, err := s.IssueEnrollmentToken("acct-A")
	if err != nil {
		t.Fatalf("IssueEnrollmentToken: %v", err)
	}
	csr, key, _ := pki.NewNodeCSR()
	certPEM, chainPEM, err := s.Enroll(tok, csr, "node-1")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	cert, err := pki.ParseCertPEM(certPEM)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	inter, err := pki.ParseCertPEM(chainPEM)
	if err != nil {
		t.Fatalf("parse chain: %v", err)
	}
	root, _ := pki.ParseCertPEM(s.RootCAPEM())
	id, err := pki.Verify(cert, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.AccountID != "acct-A" || id.NodeID != "node-1" || id.Class != pki.ClassSelfHosted {
		t.Fatalf("identity = %+v, want node-1/acct-A/self-hosted", id)
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(key.Public()) {
		t.Fatal("cert not bound to the node's key")
	}
}

// A token is single-use: a second redemption fails.
func TestEnrollTokenSingleUse(t *testing.T) {
	s := newAS(t)
	tok, _ := s.IssueEnrollmentToken("a")
	csr1, _, _ := pki.NewNodeCSR()
	csr2, _, _ := pki.NewNodeCSR()
	if _, _, err := s.Enroll(tok, csr1, "n1"); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if _, _, err := s.Enroll(tok, csr2, "n2"); err == nil {
		t.Fatal("a used enrollment token must be rejected on reuse")
	}
}

// An expired token is rejected.
func TestEnrollTokenExpires(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	s := newAS(t, authsvc.WithClock(clk.Now), authsvc.WithEnrollTokenTTL(10*time.Minute))
	tok, _ := s.IssueEnrollmentToken("a")
	clk.now = clk.now.Add(11 * time.Minute)
	csr, _, _ := pki.NewNodeCSR()
	if _, _, err := s.Enroll(tok, csr, "n"); err == nil {
		t.Fatal("an expired enrollment token must be rejected")
	}
}

// An unknown token is rejected.
func TestEnrollUnknownToken(t *testing.T) {
	s := newAS(t)
	csr, _, _ := pki.NewNodeCSR()
	if _, _, err := s.Enroll("bogus-token", csr, "n"); err == nil {
		t.Fatal("an unknown enrollment token must be rejected")
	}
}
