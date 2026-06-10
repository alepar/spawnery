package subkey

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"

	"spawnery/internal/clientverify"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
)

// Expectation is what a sealing client requires of the target node: the class
// (cloud or self-hosted) and, for self-hosted, the owning account. It is reused
// from clientverify so the SAN/tenancy check stays a single implementation.
//
// Field meaning: Tenancy is the node CLASS (pki.ClassCloud / pki.ClassSelfHosted);
// AccountID is checked only for self-hosted (a cloud node is multi-tenant). This
// matches the spec §3 "SAN matches expected (accountId | cloud, class)".
type Expectation = clientverify.Expectation

// RevocationChecker is the injected hook for the AS-published node
// revocation/deny-list (spec §1, roast M12: revocation ≠ expiry). A node may
// re-sign fresh sub-keys with its own cert key indefinitely, so validity alone
// does not revoke; clients consult this list at delivery step 2 and refuse to
// seal to a revoked node.
//
// The AS list service itself is out of scope for this slice. Default-allow
// (AllowAll) and a test double live here; the real implementation injects an
// AS-backed checker without touching this verification logic.
type RevocationChecker interface {
	// IsRevoked reports whether the node identity is revoked. A non-nil error
	// (e.g. the list could not be fetched) MUST be treated as fail-closed by the
	// caller — VerifyNodeForSealing refuses to seal on any error.
	IsRevoked(id pki.Identity) (bool, error)
}

// AllowAll is the default RevocationChecker: it revokes nothing. It is the
// behaviour before an AS revocation-list service is wired in. Production
// deployments inject a real checker.
type AllowAll struct{}

// IsRevoked always returns false (nothing revoked).
func (AllowAll) IsRevoked(pki.Identity) (bool, error) { return false, nil }

// ErrRevoked is returned when the target node is on the revocation list.
var ErrRevoked = errors.New("subkey: node is revoked (on the AS deny-list)")

// VerifyNodeForSealing is the client-side verification chain of spec §3 step 2.
// Given the CP-relayed node leaf cert (+ chain), the pinned Root CA, and the
// node's SignedSubKey, it returns the TRUSTED HPKE pubkey to seal to — or an
// error. The chain enforced, in order:
//
//  1. node cert chains to the pinned Root CA AND its SAN matches expect
//     (delegated to clientverify.VerifyHost — pinned roots + name constraints);
//  2. the node is NOT on the AS revocation list (revoked hook; fail-closed on
//     any error);
//  3. the sub-key's nodeID matches the verified cert identity;
//  4. the sub-key signature chains to the cert key (ECDSA-P256);
//  5. the sub-key is unexpired (now within [NotBefore, NotAfter)).
//
// Only when all pass is the sub-key's HPKE pubkey returned as trusted. A
// compromised CP can relay keys but cannot mint trust: it holds no CA key, so a
// forged/foreign cert fails (1); it cannot forge the cert-key signature, so a
// swapped sub-key fails (4).
func VerifyNodeForSealing(leafPEM, chainPEM, rootPEM []byte, sk SignedSubKey, expect Expectation, revoked RevocationChecker, now time.Time) (trustedHPKEPub []byte, id pki.Identity, err error) {
	// (1) chain to pinned root + SAN/tenancy match.
	id, err = clientverify.VerifyHost(leafPEM, chainPEM, rootPEM, expect, now)
	if err != nil {
		return nil, pki.Identity{}, err
	}

	// (2) revocation — fail-closed.
	if revoked == nil {
		revoked = AllowAll{}
	}
	isRevoked, err := revoked.IsRevoked(id)
	if err != nil {
		return nil, pki.Identity{}, fmt.Errorf("subkey: revocation check failed (fail-closed): %w", err)
	}
	if isRevoked {
		return nil, pki.Identity{}, ErrRevoked
	}

	// (3) sub-key bound to this node identity.
	if sk.NodeID != id.NodeID {
		return nil, pki.Identity{}, fmt.Errorf("%w: sub-key nodeID %q, cert nodeID %q", ErrNodeMismatch, sk.NodeID, id.NodeID)
	}

	// (4) sub-key signature chains to the cert key.
	leaf, err := pki.ParseCertPEM(leafPEM)
	if err != nil {
		return nil, pki.Identity{}, fmt.Errorf("subkey: parse leaf: %w", err)
	}
	certPub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, pki.Identity{}, fmt.Errorf("subkey: node cert key is %T, want *ecdsa.PublicKey", leaf.PublicKey)
	}
	if err := sk.Verify(certPub); err != nil {
		return nil, pki.Identity{}, err
	}

	// (5) sub-key unexpired.
	if err := sk.Valid(now); err != nil {
		return nil, pki.Identity{}, err
	}

	return append([]byte(nil), sk.HPKEPub...), id, nil
}

// SealForNode is the client-side delivery leg (spec §3 steps 2–3): it verifies
// the target node via VerifyNodeForSealing, then re-seals the owner envelope's
// payload to the verified node HPKE pubkey via seal.ReSealToNode under the
// in-flight AAD.
//
// The verified identity binds the AAD: NodeID and NotAfter are taken from the
// verified cert/sub-key (not the caller's aad), so the delivered ciphertext is
// cryptographically bound to the node and sub-key that were actually verified.
// The caller supplies the rest of the context (SpawnID, Generation, Version, and
// the node-issued one-time DeliveryID).
func SealForNode(env *seal.Envelope, deviceX25519Priv []byte, leafPEM, chainPEM, rootPEM []byte, sk SignedSubKey, expect Expectation, revoked RevocationChecker, aad seal.InFlightAAD, now time.Time) (*seal.NodeSealed, error) {
	hpkePub, id, err := VerifyNodeForSealing(leafPEM, chainPEM, rootPEM, sk, expect, revoked, now)
	if err != nil {
		return nil, err
	}
	aad.NodeID = id.NodeID
	aad.NotAfter = sk.NotAfter
	return seal.ReSealToNode(env, deviceX25519Priv, hpkePub, aad)
}
