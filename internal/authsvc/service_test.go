package authsvc_test

import (
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

// The AS holds the self-hosted intermediate and issues node certs that verify against the root it
// publishes for pinning — and they are always class=self-hosted, bound to the given account.
func TestServiceIssuesVerifiableSelfHostedCert(t *testing.T) {
	root, _ := pki.NewRootCA("Test Root")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	s := authsvc.New(root.Cert, inter)

	node, err := s.IssueSelfHostedNode("node-7", "acct-9", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueSelfHostedNode: %v", err)
	}

	rootCert, err := pki.ParseCertPEM(s.RootCAPEM())
	if err != nil {
		t.Fatalf("RootCAPEM/parse: %v", err)
	}
	id, err := pki.Verify(node.Cert, node.Chain, rootCert, time.Now())
	if err != nil {
		t.Fatalf("issued cert failed to verify against the published root: %v", err)
	}
	if id.Class != pki.ClassSelfHosted || id.AccountID != "acct-9" || id.NodeID != "node-7" {
		t.Fatalf("identity = %+v, want node-7/acct-9/self-hosted", id)
	}
}

// The AS loads from PEM (root + intermediate cert/key) the way it would in production, and the loaded
// service issues verifiable certs.
func TestServiceLoadFromPEM(t *testing.T) {
	root, _ := pki.NewRootCA("Test Root")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	interKeyPEM, _ := pki.MarshalKeyPEM(inter.Key)

	s, err := authsvc.Load(pki.MarshalCertPEM(root.Cert), pki.MarshalCertPEM(inter.Cert), interKeyPEM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	node, err := s.IssueSelfHostedNode("n", "a", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueSelfHostedNode: %v", err)
	}
	rootCert, _ := pki.ParseCertPEM(s.RootCAPEM())
	if _, err := pki.Verify(node.Cert, node.Chain, rootCert, time.Now()); err != nil {
		t.Fatalf("loaded-service cert failed verify: %v", err)
	}
}
