package pki

import (
	"testing"
	"time"
)

// A self-hosted leaf issued by the self-hosted intermediate verifies against the pinned root, and its
// identity (nodeId/accountId/class) is read from the SAN. Covers sp-tn9 (happy path).
func TestIssueAndVerifySelfHosted(t *testing.T) {
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

	id, err := Verify(node.Cert, node.Chain, root.Cert, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.NodeID != "node1" || id.AccountID != "acct1" || id.Class != ClassSelfHosted {
		t.Fatalf("identity = %+v, want node1/acct1/self-hosted", id)
	}
}

// THE core security property: a self-hosted intermediate may CREATE a cloud-SAN leaf (issuance is not
// constrained), but that leaf must FAIL verification against the root — name constraints make the
// self-hosted authority cryptographically incapable of producing a valid cloud identity.
func TestSelfHostedIntermediateCannotForgeCloud(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	selfHosted, _ := root.NewIntermediate(ClassSelfHosted)

	// Issuance succeeds — the constraint bites at verification, not minting.
	forged, err := selfHosted.IssueNode("evil", "victim", ClassCloud, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode (forged cloud) unexpectedly failed at mint time: %v", err)
	}
	if _, err := Verify(forged.Cert, forged.Chain, root.Cert, time.Now()); err == nil {
		t.Fatal("SECURITY: a cloud-SAN leaf signed by the self-hosted intermediate MUST fail verification")
	}
}

// A cloud leaf from the cloud intermediate verifies and reports class=cloud.
func TestIssueAndVerifyCloud(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	cloud, _ := root.NewIntermediate(ClassCloud)
	node, err := cloud.IssueNode("cnode", "spawnery-system", ClassCloud, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	id, err := Verify(node.Cert, node.Chain, root.Cert, time.Now())
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
	if _, err := Verify(node.Cert, node.Chain, root.Cert, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired leaf must be rejected")
	}
}

// A leaf does not verify against a different root (no shared trust anchor — e.g. another environment).
func TestWrongRootRejected(t *testing.T) {
	root, _ := NewRootCA("Spawnery Test Root")
	other, _ := NewRootCA("Other Root")
	inter, _ := root.NewIntermediate(ClassSelfHosted)
	node, _ := inter.IssueNode("n", "a", ClassSelfHosted, time.Now().Add(time.Hour))
	if _, err := Verify(node.Cert, node.Chain, other.Cert, time.Now()); err == nil {
		t.Fatal("leaf must not verify against a foreign root")
	}
}
