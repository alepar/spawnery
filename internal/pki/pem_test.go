package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
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
	if _, err := Verify(gotCert, []*x509.Certificate{inter.Cert}, rootCert, DefaultTrustDomain, time.Now(), allowNoCertificateRevocations); err != nil {
		t.Fatalf("reloaded cert failed verify: %v", err)
	}
}

func TestMarshalPKCS8KeyPEMAndCertificateChain(t *testing.T) {
	root, _ := NewRootCA("root")
	issuer, _ := root.NewAuthSigningIntermediate("prod")
	signer, _ := issuer.IssueAuthArtifactSigner("prod", "current", time.Now().Add(time.Hour))

	keyPEM, err := MarshalPKCS8KeyPEM(signer.Key)
	if err != nil {
		t.Fatalf("MarshalPKCS8KeyPEM: %v", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("missing private-key PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	if _, ok := key.(ed25519.PrivateKey); !ok {
		t.Fatalf("key = %T, want ed25519.PrivateKey", key)
	}

	chainPEM := MarshalCertChainPEM([]*x509.Certificate{signer.Cert, issuer.Cert})
	var got []*x509.Certificate
	for len(chainPEM) > 0 {
		var certBlock *pem.Block
		certBlock, chainPEM = pem.Decode(chainPEM)
		if certBlock == nil || certBlock.Type != "CERTIFICATE" {
			t.Fatal("invalid certificate chain PEM")
		}
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, cert)
	}
	if len(got) != 2 || !got[0].Equal(signer.Cert) || !got[1].Equal(issuer.Cert) {
		t.Fatalf("chain order = %v", got)
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
