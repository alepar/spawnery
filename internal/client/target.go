package client

import (
	"errors"
	"fmt"
	"time"

	"spawnery/internal/clientverify"
	"spawnery/internal/pki"
)

func verifyResolvedTarget(certChain []byte, targetNodeID, targetClass, targetAccountID string, trust TargetTrust) (pki.Identity, error) {
	if len(certChain) == 0 || len(trust.RootPEM) == 0 || trust.TrustDomain == "" || trust.CertificateRevocations == nil {
		return pki.Identity{}, errors.New("pinned root, trust domain, revocation state, and target certificate chain are required")
	}
	if targetNodeID == "" || targetAccountID == "" {
		return pki.Identity{}, errors.New("typed target node id and account are required")
	}
	want := clientverify.Expectation{TrustDomain: trust.TrustDomain, Tenancy: targetClass}
	switch targetClass {
	case pki.ClassSelfHosted:
		if trust.AccountID == "" || targetAccountID != trust.AccountID {
			return pki.Identity{}, fmt.Errorf("self-hosted target account %q does not match logged-in account %q", targetAccountID, trust.AccountID)
		}
		want.AccountID = trust.AccountID
	case pki.ClassCloud:
		if trust.CloudAccountID == "" || targetAccountID != trust.CloudAccountID {
			return pki.Identity{}, fmt.Errorf("cloud target account %q does not match system account %q", targetAccountID, trust.CloudAccountID)
		}
	default:
		return pki.Identity{}, fmt.Errorf("unsupported target class %q", targetClass)
	}
	leaf, chain, err := splitLeafChainPEM(certChain)
	if err != nil {
		return pki.Identity{}, err
	}
	now := time.Now()
	if trust.Now != nil {
		now = trust.Now()
	}
	id, err := clientverify.VerifyHost(leaf, chain, trust.RootPEM, want, trust.CertificateRevocations, now)
	if err != nil {
		return pki.Identity{}, err
	}
	if id.NodeID != targetNodeID || id.Class != targetClass || id.AccountID != targetAccountID {
		return pki.Identity{}, fmt.Errorf("verified principal %q/%q/%q does not match typed target %q/%q/%q", id.NodeID, id.Class, id.AccountID, targetNodeID, targetClass, targetAccountID)
	}
	return id, nil
}
