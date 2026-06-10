package pki

import (
	"crypto/ecdsa"
	"testing"
	"time"
)

// A node generates its own keypair + CSR; the CA signs the CSR's public key into a leaf with a CA-chosen
// SAN (the CA does NOT trust names requested in the CSR). The issued cert is bound to the node's key
// and verifies against the root.
func TestSignCSR(t *testing.T) {
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
	id, err := Verify(cert, chain, root.Cert, time.Now())
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
