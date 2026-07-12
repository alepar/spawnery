package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"testing"
	"time"
)

func TestSignCSRLeafProfileAndCASelectedIdentity(t *testing.T) {
	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := root.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestedURI, _ := url.Parse("spiffe://attacker.invalid/node/cloud/victim/evil")
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "attacker"},
		DNSNames:    []string{"attacker.invalid"},
		IPAddresses: nil,
		URIs:        []*url.URL{requestedURI},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := intermediate.SignNodeCSR(csrDER, "n1", "acct-1", RoleSelfHosted, "prod.spawnery.internal", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignNodeCSR: %v", err)
	}
	assertLeafProfile(t, cert, "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n1")
	if len(cert.DNSNames) != 0 || len(cert.IPAddresses) != 0 {
		t.Fatalf("CSR-requested endpoint names leaked into cert: DNS %v IP %v", cert.DNSNames, cert.IPAddresses)
	}
}

// A node generates its own keypair + CSR; the CA signs the CSR's public key into a leaf with a CA-chosen
// SAN (the CA does NOT trust names requested in the CSR). The issued cert is bound to the node's key
// and verifies against the root.
func TestCSRRoundTrip(t *testing.T) {
	root, _ := NewRootCA("R")
	inter, _ := root.NewIntermediate(ClassSelfHosted)

	csrDER, nodeKey, err := NewNodeCSR()
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}
	cert, chain, err := inter.SignCSR(csrDER, "node-x", "acct-y", ClassSelfHosted, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(nodeKey.Public()) {
		t.Fatal("issued cert is not bound to the node's keypair")
	}
	id, err := Verify(cert, chain, root.Cert, DefaultTrustDomain, time.Now(), allowNoCertificateRevocations)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.NodeID != "node-x" || id.AccountID != "acct-y" || id.Class != ClassSelfHosted {
		t.Fatalf("identity = %+v", id)
	}
}

// Garbage CSR bytes (or a tampered/unverifiable CSR) are rejected.
func TestSignCSRRejectsInvalid(t *testing.T) {
	root, _ := NewRootCA("R")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	if _, _, err := inter.SignCSR([]byte("not a csr"), "n", "a", ClassSelfHosted, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("invalid CSR must be rejected")
	}
}
