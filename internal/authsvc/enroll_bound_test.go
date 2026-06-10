package authsvc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

// boundKey makes a node key, its SPKI fingerprint, and a CSR over it — the three values the bound flow
// uses (owner binds the fingerprint; node redeems with the CSR).
func boundKey(t *testing.T) (*ecdsa.PrivateKey, string, []byte) {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	fp, err := pki.PublicKeyFingerprint(key.Public())
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	csr, err := pki.NodeCSRForKey(key)
	if err != nil {
		t.Fatalf("NodeCSRForKey: %v", err)
	}
	return key, fp, csr
}

// A fingerprint-bound token, redeemed with the CSR over the bound key, yields a self-hosted cert bound to
// the token's account + the node's own key.
func TestBoundTokenBindsFingerprint(t *testing.T) {
	s := newAS(t)
	key, fp, csr := boundKey(t)
	tok, err := s.IssueBoundEnrollmentToken("acct-A", pki.ClassSelfHosted, fp)
	if err != nil {
		t.Fatalf("IssueBoundEnrollmentToken: %v", err)
	}
	certPEM, chainPEM, err := s.Enroll(tok, csr, "node-1")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	cert, _ := pki.ParseCertPEM(certPEM)
	inter, _ := pki.ParseCertPEM(chainPEM)
	root, _ := pki.ParseCertPEM(s.RootCAPEM())
	id, err := pki.Verify(cert, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.AccountID != "acct-A" || id.NodeID != "node-1" || id.Class != pki.ClassSelfHosted {
		t.Fatalf("identity = %+v", id)
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(key.Public()) {
		t.Fatal("cert not bound to the node's key")
	}
}

// THE CORE PROPERTY: a token bound to key K1's fingerprint cannot be redeemed with a different key K2 —
// even though K2's CSR is otherwise valid. A leaked/relayed token + substituted key is rejected. And the
// failed attempt does NOT consume the token: the legitimate node (K1) can still redeem afterwards.
func TestBoundTokenRejectsSubstitutedKey(t *testing.T) {
	s := newAS(t)
	_, fp1, csr1 := boundKey(t)
	_, _, csr2 := boundKey(t) // a different key's CSR
	tok, _ := s.IssueBoundEnrollmentToken("acct-A", pki.ClassSelfHosted, fp1)

	_, _, err := s.Enroll(tok, csr2, "attacker-node")
	if !errors.Is(err, authsvc.ErrTokenFingerprintMismatch) {
		t.Fatalf("substituted key: got %v, want ErrTokenFingerprintMismatch", err)
	}
	// The token must remain usable by the legitimate node — the thief's attempt didn't burn it.
	if _, _, err := s.Enroll(tok, csr1, "node-1"); err != nil {
		t.Fatalf("legitimate redemption after a rejected substitution: %v", err)
	}
}

// A bound token is single-use: a second redemption with the same (valid) key fails.
func TestBoundTokenSingleUse(t *testing.T) {
	s := newAS(t)
	_, fp, csr := boundKey(t)
	tok, _ := s.IssueBoundEnrollmentToken("a", pki.ClassSelfHosted, fp)
	if _, _, err := s.Enroll(tok, csr, "n1"); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if _, _, err := s.Enroll(tok, csr, "n1"); err == nil {
		t.Fatal("a used bound token must be rejected on reuse")
	}
}

// An expired bound token is rejected.
func TestBoundTokenExpires(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	s := newAS(t, authsvc.WithClock(clk.Now), authsvc.WithEnrollTokenTTL(10*time.Minute))
	_, fp, csr := boundKey(t)
	tok, _ := s.IssueBoundEnrollmentToken("a", pki.ClassSelfHosted, fp)
	clk.now = clk.now.Add(11 * time.Minute)
	if _, _, err := s.Enroll(tok, csr, "n"); err == nil {
		t.Fatal("an expired bound token must be rejected")
	}
}

// Class scoping: the AS holds only the self-hosted intermediate, so it refuses to mint a token bound to
// class=cloud — escalation to the multi-tenant class is impossible at the source.
func TestBoundTokenRejectsCloudClass(t *testing.T) {
	s := newAS(t)
	_, fp, _ := boundKey(t)
	if _, err := s.IssueBoundEnrollmentToken("a", pki.ClassCloud, fp); !errors.Is(err, authsvc.ErrUnsignableClass) {
		t.Fatalf("cloud class: got %v, want ErrUnsignableClass", err)
	}
}

// A bound token requires a non-empty fingerprint (an empty one would degrade to the unbound behaviour).
func TestBoundTokenRequiresFingerprint(t *testing.T) {
	s := newAS(t)
	if _, err := s.IssueBoundEnrollmentToken("a", pki.ClassSelfHosted, ""); err == nil {
		t.Fatal("a bound token with an empty fingerprint must be rejected")
	}
}

// Account scoping: the issued cert's account comes from the TOKEN, never the redeemer — a token bound to
// acct-A can only ever yield an acct-A identity.
func TestBoundTokenAccountScoping(t *testing.T) {
	s := newAS(t)
	_, fp, csr := boundKey(t)
	tok, _ := s.IssueBoundEnrollmentToken("acct-A", pki.ClassSelfHosted, fp)
	certPEM, chainPEM, err := s.Enroll(tok, csr, "node-1")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	cert, _ := pki.ParseCertPEM(certPEM)
	inter, _ := pki.ParseCertPEM(chainPEM)
	root, _ := pki.ParseCertPEM(s.RootCAPEM())
	id, err := pki.Verify(cert, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.AccountID != "acct-A" {
		t.Fatalf("account = %q, want acct-A (from the token)", id.AccountID)
	}
}

// HTTP bound round-trip: the node redeems a fingerprint-bound token with its pre-generated key via
// RunEnrollWithKey, and a DIFFERENT key is rejected over the wire (401 -> error).
func TestBoundEnrollHTTPRoundTrip(t *testing.T) {
	s := newAS(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	key, fp, _ := boundKey(t)
	tok, _ := s.IssueBoundEnrollmentToken("acct-Z", pki.ClassSelfHosted, fp)

	res, err := authsvc.RunEnrollWithKey(context.Background(), srv.URL, tok, "node-q", key)
	if err != nil {
		t.Fatalf("RunEnrollWithKey: %v", err)
	}
	cert, _ := pki.ParseCertPEM(res.CertPEM)
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(key.Public()) {
		t.Fatal("issued cert not bound to the redeeming key")
	}

	// A token bound to fp(key) cannot be redeemed with a fresh, different key over HTTP.
	other, _ := pki.NewNodeKey()
	tok2, _ := s.IssueBoundEnrollmentToken("acct-Z", pki.ClassSelfHosted, fp)
	if _, err := authsvc.RunEnrollWithKey(context.Background(), srv.URL, tok2, "node-q", other); err == nil {
		t.Fatal("a bound token redeemed with a substituted key must fail over HTTP")
	}
}
