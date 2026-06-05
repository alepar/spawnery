package pki

import (
	"crypto/x509"
	"testing"
	"time"
)

// Certs and keys round-trip through PEM so the AS can persist the intermediate, nodes can store their
// leaf, and the CP can load the pinned root.
func TestPEMRoundTrip(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	node, _ := inter.IssueNode("n1", "a1", ClassSelfHosted, time.Now().Add(time.Hour))

	certPEM := MarshalCertPEM(node.Cert)
	keyPEM, err := MarshalKeyPEM(node.Key)
	if err != nil {
		t.Fatalf("MarshalKeyPEM: %v", err)
	}

	gotCert, err := ParseCertPEM(certPEM)
	if err != nil {
		t.Fatalf("ParseCertPEM: %v", err)
	}
	if gotCert.Subject.CommonName != node.Cert.Subject.CommonName {
		t.Fatalf("cert CN = %q, want %q", gotCert.Subject.CommonName, node.Cert.Subject.CommonName)
	}
	gotKey, err := ParseKeyPEM(keyPEM)
	if err != nil {
		t.Fatalf("ParseKeyPEM: %v", err)
	}
	if !gotKey.PublicKey.Equal(node.Key.Public()) {
		t.Fatal("round-tripped key does not match")
	}

	// The reloaded cert still verifies against the (reloaded) root.
	rootCert, err := ParseCertPEM(MarshalCertPEM(root.Cert))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(gotCert, []*x509.Certificate{inter.Cert}, rootCert, time.Now()); err != nil {
		t.Fatalf("reloaded cert failed verify: %v", err)
	}
}

// TLSCertificate yields a usable mTLS identity (leaf+chain+key) for dialing/serving.
func TestTLSCertificate(t *testing.T) {
	root, _ := NewRootCA("R")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	node, _ := inter.IssueNode("n", "a", ClassSelfHosted, time.Now().Add(time.Hour))
	tc, err := node.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if len(tc.Certificate) < 2 {
		t.Fatalf("want leaf+intermediate in the chain, got %d", len(tc.Certificate))
	}
	if tc.PrivateKey == nil {
		t.Fatal("private key not set")
	}
}
