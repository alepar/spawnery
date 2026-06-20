package main

import (
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

// TestGenDevCloudNodeIdentity verifies that genDev emits a cloud node identity under node-cloud/
// that chains to the dev root CA and reports class=cloud, nodeID=node-1, accountID=spawnery-system.
func TestGenDevCloudNodeIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}

	id, err := nodeid.Load(filepath.Join(dir, "node-cloud"))
	if err != nil {
		t.Fatalf("nodeid.Load(node-cloud): %v", err)
	}

	leaf, err := pki.ParseCertPEM(id.CertPEM)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	inter, err := pki.ParseCertPEM(id.ChainPEM)
	if err != nil {
		t.Fatalf("parse chain cert: %v", err)
	}
	root, err := pki.ParseCertPEM(id.RootPEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}

	identity, err := pki.Verify(leaf, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("Verify cloud node identity: %v", err)
	}
	if identity.Class != pki.ClassCloud {
		t.Errorf("class = %q, want %q", identity.Class, pki.ClassCloud)
	}
	if identity.AccountID != "spawnery-system" {
		t.Errorf("accountID = %q, want %q", identity.AccountID, "spawnery-system")
	}
	if identity.NodeID != "node-1" {
		t.Errorf("nodeID = %q, want %q", identity.NodeID, "node-1")
	}
}

// TestGenDevSelfHostedNodeIdentityStillPresent verifies that the self-hosted node identity
// (used by dev-enforced / node-enforced) is still emitted by genDev unchanged.
func TestGenDevSelfHostedNodeIdentityStillPresent(t *testing.T) {
	dir := t.TempDir()
	if err := genDev(dir); err != nil {
		t.Fatalf("genDev: %v", err)
	}

	id, err := nodeid.Load(filepath.Join(dir, "node"))
	if err != nil {
		t.Fatalf("nodeid.Load(node): %v", err)
	}

	leaf, err := pki.ParseCertPEM(id.CertPEM)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	inter, err := pki.ParseCertPEM(id.ChainPEM)
	if err != nil {
		t.Fatalf("parse chain cert: %v", err)
	}
	root, err := pki.ParseCertPEM(id.RootPEM)
	if err != nil {
		t.Fatalf("parse root cert: %v", err)
	}

	identity, err := pki.Verify(leaf, []*x509.Certificate{inter}, root, time.Now())
	if err != nil {
		t.Fatalf("Verify self-hosted node identity: %v", err)
	}
	if identity.Class != pki.ClassSelfHosted {
		t.Errorf("class = %q, want %q", identity.Class, pki.ClassSelfHosted)
	}
	if identity.AccountID != "alice" {
		t.Errorf("accountID = %q, want %q", identity.AccountID, "alice")
	}
	if identity.NodeID != "node-1" {
		t.Errorf("nodeID = %q, want %q", identity.NodeID, "node-1")
	}
}
