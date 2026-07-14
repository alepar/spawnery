package subkey

import (
	"crypto/ecdsa"
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

// VerifyNodeForSealing is the client-side verification chain of spec §3 step 2.
// Given the CP-relayed node leaf cert (+ chain), the pinned Root CA, and the
// node's SignedSubKey, it returns the TRUSTED HPKE pubkey to seal to — or an
// error. The chain enforced, in order:
//
//  1. node cert chains to the pinned Root CA AND its SAN matches expect
//     (delegated to clientverify.VerifyHost — pinned roots + issuer policy/path correspondence);
//  2. the sub-key's nodeID matches the verified cert identity;
//  3. the sub-key signature chains to the cert key (ECDSA-P256);
//  4. the sub-key is unexpired (now within [NotBefore, NotAfter)).
//
// Only when all pass is the sub-key's HPKE pubkey returned as trusted. A
// compromised CP can relay keys but cannot mint trust: it holds no CA key, so a
// forged/foreign cert fails (1); it cannot forge the cert-key signature, so a
// swapped sub-key fails (4).
func VerifyNodeForSealing(leafPEM, chainPEM, rootPEM []byte, sk SignedSubKey, expect Expectation, certificateRevocations pki.CertificateRevocationChecker, now time.Time) (trustedHPKEPub []byte, id pki.Identity, err error) {
	// (1) chain to pinned root + SAN/tenancy match.
	id, err = clientverify.VerifyHost(leafPEM, chainPEM, rootPEM, expect, certificateRevocations, now)
	if err != nil {
		return nil, pki.Identity{}, err
	}

	// (2) sub-key bound to this node identity.
	if sk.NodeID != id.NodeID {
		return nil, pki.Identity{}, fmt.Errorf("%w: sub-key nodeID %q, cert nodeID %q", ErrNodeMismatch, sk.NodeID, id.NodeID)
	}

	// (3) sub-key signature chains to the cert key.
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

	// (4) sub-key unexpired.
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
func SealForNode(env *seal.Envelope, deviceX25519Priv []byte, leafPEM, chainPEM, rootPEM []byte, sk SignedSubKey, expect Expectation, certificateRevocations pki.CertificateRevocationChecker, aad seal.InFlightAAD, now time.Time) (*seal.NodeSealed, error) {
	hpkePub, id, err := VerifyNodeForSealing(leafPEM, chainPEM, rootPEM, sk, expect, certificateRevocations, now)
	if err != nil {
		return nil, err
	}
	aad.NodeID = id.NodeID
	aad.NotAfter = sk.NotAfter
	return seal.ReSealToNode(env, deviceX25519Priv, hpkePub, aad)
}
