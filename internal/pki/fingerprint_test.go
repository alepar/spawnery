package pki

import "testing"

// A key's SPKI fingerprint is stable, and the fingerprint derived from a CSR over that key equals the
// fingerprint of the key itself — so the owner (from the key) and the AS (from the CSR) agree.
func TestFingerprintStableAndCSRMatchesKey(t *testing.T) {
	key, err := NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	fp1, err := PublicKeyFingerprint(key.Public())
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	fp2, _ := PublicKeyFingerprint(key.Public())
	if fp1 != fp2 || fp1 == "" {
		t.Fatalf("fingerprint not stable/non-empty: %q vs %q", fp1, fp2)
	}
	csrDER, err := NodeCSRForKey(key)
	if err != nil {
		t.Fatalf("NodeCSRForKey: %v", err)
	}
	csrFP, err := CSRPublicKeyFingerprint(csrDER)
	if err != nil {
		t.Fatalf("CSRPublicKeyFingerprint: %v", err)
	}
	if csrFP != fp1 {
		t.Fatalf("CSR fingerprint %q != key fingerprint %q", csrFP, fp1)
	}
}

// Different keys produce different fingerprints (the binding actually discriminates).
func TestFingerprintDistinctKeys(t *testing.T) {
	k1, _ := NewNodeKey()
	k2, _ := NewNodeKey()
	fp1, _ := PublicKeyFingerprint(k1.Public())
	fp2, _ := PublicKeyFingerprint(k2.Public())
	if fp1 == fp2 {
		t.Fatal("distinct keys produced the same fingerprint")
	}
}

// A garbage CSR is rejected by the fingerprint helper.
func TestCSRFingerprintRejectsGarbage(t *testing.T) {
	if _, err := CSRPublicKeyFingerprint([]byte("not a csr")); err == nil {
		t.Fatal("garbage CSR bytes must be rejected")
	}
}
