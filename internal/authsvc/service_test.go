package authsvc_test

import (
	"testing"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
)

func TestIssueSelfHostedNodeUsesConfiguredTrustDomain(t *testing.T) {
	root, _ := pki.NewRootCA("root")
	intermediate, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, "prod.spawnery.internal")
	service := authsvc.New(root.Cert, intermediate, authsvc.WithTrustDomain("prod.spawnery.internal"))
	leaf, err := service.IssueSelfHostedNode("n", "a", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := leaf.Cert.URIs[0].String(); got != "spiffe://prod.spawnery.internal/node/self-hosted/a/n" {
		t.Fatalf("URI SAN = %q", got)
	}
}

func TestServiceValidateRejectsInvalidTrustDomain(t *testing.T) {
	root, _ := pki.NewRootCA("root")
	intermediate, _ := root.NewIntermediate(pki.IssuerSelfHostedNode, pki.DefaultTrustDomain)
	service := authsvc.New(root.Cert, intermediate, authsvc.WithTrustDomain("INVALID domain"))
	if err := service.Validate(); err == nil {
		t.Fatal("invalid trust domain accepted")
	}
}

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
	id, err := pki.Verify(node.Cert, node.Chain, rootCert, pki.DefaultTrustDomain, time.Now())
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

	s, err := authsvc.Load(pki.MarshalCertPEM(root.Cert), pki.MarshalCertPEM(inter.Cert), interKeyPEM, pki.DefaultTrustDomain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	node, err := s.IssueSelfHostedNode("n", "a", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueSelfHostedNode: %v", err)
	}
	rootCert, _ := pki.ParseCertPEM(s.RootCAPEM())
	if _, err := pki.Verify(node.Cert, node.Chain, rootCert, pki.DefaultTrustDomain, time.Now()); err != nil {
		t.Fatalf("loaded-service cert failed verify: %v", err)
	}
}

func TestServiceLoadRejectsInvalidTrustDomain(t *testing.T) {
	root, _ := pki.NewRootCA("Test Root")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	interKeyPEM, _ := pki.MarshalKeyPEM(inter.Key)
	if _, err := authsvc.Load(pki.MarshalCertPEM(root.Cert), pki.MarshalCertPEM(inter.Cert), interKeyPEM, "INVALID domain"); err == nil {
		t.Fatal("Load accepted invalid trust domain")
	}
}
