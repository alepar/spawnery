// Package clientverify is the client-side infra check (sp-ova design §7): before a client (spawnctl, a
// node acting as a client, or a natively-delivered web bundle) trusts the node hosting its workload, it
// verifies that node's certificate chains to the PINNED Root CA and that its SAN identity matches what
// the client expects — self-hosted bound to the client's own account, or a cloud (multi-tenant) node.
// This lets a CP-only compromise be DETECTED: a compromised CP cannot present a node cert for the
// victim's account/class that chains to the pinned root, because it holds no CA key.
package clientverify

import (
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"spawnery/internal/pki"
)

// Expectation is what the client requires of the hosting node. Tenancy is "self-hosted" or "cloud"; for
// self-hosted, AccountID must be the client's own account.
type Expectation struct {
	Tenancy   string
	AccountID string
}

// VerifyHost verifies the host node's cert chain against the pinned root and checks its SAN identity
// against the expectation. It returns the verified identity, or an error if the chain is invalid or the
// identity is not the expected host.
func VerifyHost(leafPEM, chainPEM, rootPEM []byte, want Expectation, now time.Time) (pki.Identity, error) {
	leaf, err := pki.ParseCertPEM(leafPEM)
	if err != nil {
		return pki.Identity{}, fmt.Errorf("clientverify: leaf: %w", err)
	}
	root, err := pki.ParseCertPEM(rootPEM)
	if err != nil {
		return pki.Identity{}, fmt.Errorf("clientverify: pinned root: %w", err)
	}
	var chain []*x509.Certificate
	if len(chainPEM) > 0 {
		c, err := pki.ParseCertPEM(chainPEM)
		if err != nil {
			return pki.Identity{}, fmt.Errorf("clientverify: chain: %w", err)
		}
		chain = append(chain, c)
	}
	id, err := pki.Verify(leaf, chain, root, now)
	if err != nil {
		return pki.Identity{}, fmt.Errorf("clientverify: host cert does not chain to the pinned root: %w", err)
	}
	switch want.Tenancy {
	case pki.ClassCloud:
		if id.Class != pki.ClassCloud {
			return pki.Identity{}, fmt.Errorf("clientverify: expected a cloud host, got class %q", id.Class)
		}
	case pki.ClassSelfHosted:
		if id.Class != pki.ClassSelfHosted {
			return pki.Identity{}, fmt.Errorf("clientverify: expected a self-hosted host, got class %q", id.Class)
		}
		if id.AccountID != want.AccountID {
			return pki.Identity{}, fmt.Errorf("clientverify: self-hosted host bound to %q, not my account %q", id.AccountID, want.AccountID)
		}
	default:
		return pki.Identity{}, errors.New("clientverify: expectation Tenancy must be cloud or self-hosted")
	}
	return id, nil
}
