package pki

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestIssueLeafProfile(t *testing.T) {
	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := root.NewIntermediate(IssuerSelfHostedNode, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := intermediate.IssueNode("n1", "acct-1", RoleSelfHosted, "prod.spawnery.internal", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	assertLeafProfile(t, leaf.Cert, "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n1")
}

func TestIssueServiceLeafProfile(t *testing.T) {
	root, err := NewRootCA("root")
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := root.NewIntermediate(IssuerService, "prod.spawnery.internal")
	if err != nil {
		t.Fatal(err)
	}
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	leaf, err := intermediate.IssueService(RoleCP, "cp-a", "prod.spawnery.internal", []string{"cp.internal"}, ips, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueService: %v", err)
	}
	assertLeafProfile(t, leaf.Cert, "spiffe://prod.spawnery.internal/service/cp/cp-a")
	if len(leaf.Cert.DNSNames) != 1 || leaf.Cert.DNSNames[0] != "cp.internal" {
		t.Fatalf("DNS SANs = %v", leaf.Cert.DNSNames)
	}
	if len(leaf.Cert.IPAddresses) != 1 || !leaf.Cert.IPAddresses[0].Equal(ips[0]) {
		t.Fatalf("IP SANs = %v", leaf.Cert.IPAddresses)
	}
}

func assertLeafProfile(t *testing.T, cert *x509.Certificate, wantURI string) {
	t.Helper()
	if cert.IsCA || !cert.BasicConstraintsValid {
		t.Fatalf("leaf basic constraints = valid %v CA %v", cert.BasicConstraintsValid, cert.IsCA)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("key usage = %v, want DigitalSignature only", cert.KeyUsage)
	}
	if len(cert.ExtKeyUsage) != 2 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || cert.ExtKeyUsage[1] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("extended key usages = %v", cert.ExtKeyUsage)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != wantURI {
		t.Fatalf("URI SANs = %v, want %q", cert.URIs, wantURI)
	}
}

// A self-hosted leaf issued by the self-hosted intermediate verifies against the pinned root, and its
// identity (nodeId/accountId/class) is read from the SAN. Covers sp-tn9 (happy path).
func TestVerifySelfHosted(t *testing.T) {
	root, err := NewRootCA("Spawnery Test Root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	inter, err := root.NewIntermediate(ClassSelfHosted)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	node, err := inter.IssueNode("node1", "acct1", ClassSelfHosted, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}

	id, err := Verify(node.Cert, node.Chain, root.Cert, DefaultTrustDomain, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.NodeID != "node1" || id.AccountID != "acct1" || id.Class != ClassSelfHosted {
		t.Fatalf("identity = %+v, want node1/acct1/self-hosted", id)
	}
}

// THE core security property: a self-hosted intermediate may CREATE a cloud-path leaf, but the
// root-signed issuer policy and path correspondence must make verification fail.
func TestSelfHostedIntermediateCannotForgeCloud(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	selfHosted, _ := root.NewIntermediate(ClassSelfHosted)

	// Issuance succeeds — the constraint bites at verification, not minting.
	forged, err := selfHosted.IssueNode("evil", "victim", ClassCloud, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode (forged cloud) unexpectedly failed at mint time: %v", err)
	}
	if _, err := Verify(forged.Cert, forged.Chain, root.Cert, DefaultTrustDomain, time.Now()); err == nil {
		t.Fatal("SECURITY: a cloud-SAN leaf signed by the self-hosted intermediate MUST fail verification")
	}
}

// A cloud leaf from the cloud intermediate verifies and reports class=cloud.
func TestVerifyCloud(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	cloud, _ := root.NewIntermediate(ClassCloud)
	node, err := cloud.IssueNode("cnode", "spawnery-system", ClassCloud, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	id, err := Verify(node.Cert, node.Chain, root.Cert, DefaultTrustDomain, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Class != ClassCloud || id.AccountID != "spawnery-system" {
		t.Fatalf("identity = %+v, want cloud/spawnery-system", id)
	}
}

// An expired leaf is rejected.
func TestExpiredLeafRejected(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	node, _ := inter.IssueNode("n", "a", ClassSelfHosted, time.Now().Add(time.Hour))
	if _, err := Verify(node.Cert, node.Chain, root.Cert, DefaultTrustDomain, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired leaf must be rejected")
	}
}

// A leaf does not verify against a different root (no shared trust anchor — e.g. another environment).
func TestWrongRootRejected(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	other, _ := NewRootCA("Other Root")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	node, _ := inter.IssueNode("n", "a", ClassSelfHosted, time.Now().Add(time.Hour))
	if _, err := Verify(node.Cert, node.Chain, other.Cert, DefaultTrustDomain, time.Now()); err == nil {
		t.Fatal("leaf must not verify against a foreign root")
	}
}
