package clientverify_test

import (
	"testing"
	"time"

	"spawnery/internal/clientverify"
	"spawnery/internal/pki"
)

// host issues a node identity and returns the PEMs a client would receive + its pinned root.
func host(t *testing.T, interClass, sanClass, account string) (leaf, chain, root []byte) {
	t.Helper()
	r, _ := pki.NewRootCA("R")
	inter, _ := r.NewIntermediate(interClass)
	node, _ := inter.IssueNode("n", account, sanClass, time.Now().Add(time.Hour))
	return pki.MarshalCertPEM(node.Cert), pki.MarshalCertPEM(inter.Cert), pki.MarshalCertPEM(r.Cert)
}

// A self-hosted host bound to my own account is accepted.
func TestVerifyHostSelfHostedMine(t *testing.T) {
	leaf, chain, root := host(t, pki.ClassSelfHosted, pki.ClassSelfHosted, "alice")
	id, err := clientverify.VerifyHost(leaf, chain, root,
		clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"}, time.Now())
	if err != nil {
		t.Fatalf("VerifyHost: %v", err)
	}
	if id.AccountID != "alice" {
		t.Fatalf("id = %+v", id)
	}
}

// SECURITY (§7): a self-hosted host bound to ANOTHER owner's account (a compromised CP routing my
// workload to an attacker's node) is rejected.
func TestVerifyHostRejectsForeignSelfHosted(t *testing.T) {
	leaf, chain, root := host(t, pki.ClassSelfHosted, pki.ClassSelfHosted, "attacker")
	if _, err := clientverify.VerifyHost(leaf, chain, root,
		clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"}, time.Now()); err == nil {
		t.Fatal("a self-hosted node bound to a different account must be rejected")
	}
}

// A cloud host (multi-tenant) is accepted for a cloud expectation.
func TestVerifyHostCloud(t *testing.T) {
	leaf, chain, root := host(t, pki.ClassCloud, pki.ClassCloud, "spawnery-system")
	if _, err := clientverify.VerifyHost(leaf, chain, root,
		clientverify.Expectation{Tenancy: pki.ClassCloud}, time.Now()); err != nil {
		t.Fatalf("cloud host should verify: %v", err)
	}
}

// SECURITY: a cloud-SAN leaf forged by a self-hosted intermediate is rejected (name constraints).
func TestVerifyHostRejectsForgedCloud(t *testing.T) {
	leaf, chain, root := host(t, pki.ClassSelfHosted, pki.ClassCloud, "spawnery-system")
	if _, err := clientverify.VerifyHost(leaf, chain, root,
		clientverify.Expectation{Tenancy: pki.ClassCloud}, time.Now()); err == nil {
		t.Fatal("a forged cloud host must be rejected")
	}
}

// A class mismatch (expect cloud, got self-hosted) is rejected.
func TestVerifyHostRejectsClassMismatch(t *testing.T) {
	leaf, chain, root := host(t, pki.ClassSelfHosted, pki.ClassSelfHosted, "alice")
	if _, err := clientverify.VerifyHost(leaf, chain, root,
		clientverify.Expectation{Tenancy: pki.ClassCloud}, time.Now()); err == nil {
		t.Fatal("expecting a cloud host but given a self-hosted one must be rejected")
	}
}
