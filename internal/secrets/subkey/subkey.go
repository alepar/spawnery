// Package subkey implements the cert-signed node HPKE sub-key and the
// client-side verification layer that turns "a trusted node HPKE pubkey" (which
// internal/secrets/seal assumes as already-trusted input) into something
// actually verified against pinned roots.
//
// It composes the already-merged building blocks rather than reinventing them:
//   - internal/secrets/seal — the HPKE envelope + ReSealToNode/OpenFromOwner.
//   - internal/pki          — the name-constrained CA chain + cert helpers.
//   - internal/clientverify — the client-side infra/SAN verification primitive.
//
// Design: docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §1
// (node sub-key + revocation) and §3 (delivery flow + verification chain).
//
// The pattern is RFC 9345 delegated credentials / Signal signed-prekeys: a node
// generates an X25519 HPKE sub-keypair and publishes the pubkey in a small
// SignedSubKey structure signed by its cert key (ECDSA P-256, the key type the
// node leaf cert uses — see internal/pki) with an expiry. A sealing client
// verifies that signature chains to the pinned Root CA via the node leaf cert,
// so a compromised CP can relay keys but cannot mint trust.
package subkey

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"spawnery/internal/secrets/seal"
)

// DefaultValidity is the sub-key lifetime (spec §1: "Validity 72 h, rotate at
// half-life").
const DefaultValidity = 72 * time.Hour

// MaxRetained is the number of unexpired sub-key private halves a node keeps so
// a rotation mid-delivery does not fail opaquely (spec §1, roast m2).
const MaxRetained = 2

// sigDomain domain-separates the sub-key signature so a node cert signature over
// some other structure can never be replayed as a sub-key signature.
const sigDomain = "spawnery/secrets/subkey/v1"

// Errors returned by this package.
var (
	ErrBadHPKEPub   = errors.New("subkey: malformed HPKE pubkey (want 32 bytes)")
	ErrBadSig       = errors.New("subkey: signature does not verify against the node cert key")
	ErrNotYetValid  = errors.New("subkey: not yet valid (notBefore > now)")
	ErrExpired      = errors.New("subkey: expired (notAfter <= now)")
	ErrNodeMismatch = errors.New("subkey: sub-key nodeID does not match the verified cert identity")
	ErrNoSubKey     = errors.New("subkey: node has no unexpired sub-key")
	ErrNoMatch      = errors.New("subkey: no retained sub-key opens this delivery")
)

const x25519KeySize = 32

// SignedSubKey is a node's published HPKE sub-key: the raw X25519 pubkey, the
// validity window, the owning nodeID, and an ECDSA-P256 signature by the node's
// cert private key over all of those. It is relayed (untrusted) by the CP; a
// sealing client verifies Sig against the verified node leaf cert's public key.
type SignedSubKey struct {
	HPKEPub   []byte    `json:"hpke_pub"`   // raw 32-byte X25519 (seal/HPKE) pubkey
	NodeID    string    `json:"node_id"`    // must equal the cert SAN's nodeID
	NotBefore time.Time `json:"not_before"` // validity start
	NotAfter  time.Time `json:"not_after"`  // validity end (bound into delivery AAD)
	Sig       []byte    `json:"sig"`        // ECDSA-P256 ASN.1 sig over signedBytes()
}

// signedBytes is the canonical, unambiguous byte string the signature covers.
func (s SignedSubKey) signedBytes() []byte {
	return encodeFields(
		[]byte(sigDomain),
		s.HPKEPub,
		[]byte(s.NodeID),
		u64(uint64(s.NotBefore.UnixNano())),
		u64(uint64(s.NotAfter.UnixNano())),
	)
}

// KeyID is a stable short identifier for this sub-key (SHA-256 of the pubkey,
// base64url, first 16 bytes). Useful for logs and for de-duplicating retained
// keys; selection on Open is by trial-Open (see Node.OpenDelivered).
func (s SignedSubKey) KeyID() string {
	sum := sha256.Sum256(s.HPKEPub)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// Sign builds a SignedSubKey for hpkePub over [notBefore, notAfter) bound to
// nodeID, signed by the node's cert private key (signKey).
func Sign(signKey *ecdsa.PrivateKey, nodeID string, hpkePub []byte, notBefore, notAfter time.Time) (SignedSubKey, error) {
	if len(hpkePub) != x25519KeySize {
		return SignedSubKey{}, ErrBadHPKEPub
	}
	if signKey == nil {
		return SignedSubKey{}, errors.New("subkey: nil signing key")
	}
	s := SignedSubKey{
		HPKEPub:   append([]byte(nil), hpkePub...),
		NodeID:    nodeID,
		NotBefore: notBefore,
		NotAfter:  notAfter,
	}
	digest := sha256.Sum256(s.signedBytes())
	sig, err := ecdsa.SignASN1(rand.Reader, signKey, digest[:])
	if err != nil {
		return SignedSubKey{}, fmt.Errorf("subkey: sign: %w", err)
	}
	s.Sig = sig
	return s, nil
}

// Verify checks the sub-key's signature against certPub (the node leaf cert's
// public key). It does NOT check expiry — that is a separate, clock-injected
// step (see VerifyNodeForSealing / Valid).
func (s SignedSubKey) Verify(certPub *ecdsa.PublicKey) error {
	if certPub == nil {
		return errors.New("subkey: nil cert public key")
	}
	if len(s.HPKEPub) != x25519KeySize {
		return ErrBadHPKEPub
	}
	digest := sha256.Sum256(s.signedBytes())
	if !ecdsa.VerifyASN1(certPub, digest[:], s.Sig) {
		return ErrBadSig
	}
	return nil
}

// Valid reports whether now is within [NotBefore, NotAfter). It is the expiry
// half of verification, kept separate from the signature check so callers can
// inject now.
func (s SignedSubKey) Valid(now time.Time) error {
	if now.Before(s.NotBefore) {
		return ErrNotYetValid
	}
	if !now.Before(s.NotAfter) {
		return ErrExpired
	}
	return nil
}

// Node is the node-side holder of HPKE sub-keys: it generates and re-signs
// sub-keys with the node cert key, retains up to MaxRetained unexpired private
// halves, and opens delivered ciphertexts by trial-Open across them.
type Node struct {
	signKey  *ecdsa.PrivateKey
	nodeID   string
	validity time.Duration
	retained []retained // newest first
}

type retained struct {
	signed SignedSubKey
	priv   []byte // raw 32-byte X25519 private half
}

// NewNode constructs a holder for nodeID signing with signKey (the node cert
// private key) and the given sub-key validity. Call Rotate to mint the first
// sub-key. If validity <= 0, DefaultValidity is used.
func NewNode(signKey *ecdsa.PrivateKey, nodeID string, validity time.Duration) *Node {
	if validity <= 0 {
		validity = DefaultValidity
	}
	return &Node{signKey: signKey, nodeID: nodeID, validity: validity}
}

// Rotate generates a fresh X25519 sub-keypair, signs it for [now, now+validity),
// retains its private half, prunes expired halves, caps retention at
// MaxRetained, and returns the newly minted SignedSubKey to publish.
func (n *Node) Rotate(now time.Time) (SignedSubKey, error) {
	pub, priv, err := seal.NodeKeyPair()
	if err != nil {
		return SignedSubKey{}, err
	}
	signed, err := Sign(n.signKey, n.nodeID, pub, now, now.Add(n.validity))
	if err != nil {
		return SignedSubKey{}, err
	}
	// Newest first.
	n.retained = append([]retained{{signed: signed, priv: priv}}, n.retained...)
	n.prune(now)
	return signed, nil
}

// prune drops expired retained halves and caps the rest (newest-first) at
// MaxRetained.
func (n *Node) prune(now time.Time) {
	kept := n.retained[:0]
	for _, r := range n.retained {
		if r.signed.Valid(now) == nil && len(kept) < MaxRetained {
			kept = append(kept, r)
		}
	}
	n.retained = kept
}

// Current returns the freshest unexpired SignedSubKey to publish, if any.
func (n *Node) Current(now time.Time) (SignedSubKey, bool) {
	for _, r := range n.retained {
		if r.signed.Valid(now) == nil {
			return r.signed, true
		}
	}
	return SignedSubKey{}, false
}

// NeedsRotation reports whether the freshest sub-key is past its half-life (or
// there is no unexpired sub-key) — the cue to call Rotate (spec §1: rotate at
// half-life).
func (n *Node) NeedsRotation(now time.Time) bool {
	cur, ok := n.Current(now)
	if !ok {
		return true
	}
	halfLife := cur.NotBefore.Add(cur.NotAfter.Sub(cur.NotBefore) / 2)
	return !now.Before(halfLife)
}

// Retained returns the count of unexpired retained sub-key private halves.
func (n *Node) Retained(now time.Time) int {
	c := 0
	for _, r := range n.retained {
		if r.signed.Valid(now) == nil {
			c++
		}
	}
	return c
}

// OpenDelivered opens a CP-relayed NodeSealed by trial-Open across the retained,
// unexpired sub-key private halves: for each, it reconstructs the expected
// in-flight AAD with THAT sub-key's NotAfter (and this node's NodeID) and calls
// seal.OpenFromOwner. The first that opens wins, so a rotation mid-delivery
// (two concurrent sub-keys) does not fail opaquely (spec §1, roast m2).
//
// baseAAD carries the per-delivery context the node knows out-of-band (spawnId,
// generation, version, deliveryId); NodeID and NotAfter are supplied per-key so
// the node need not know out-of-band which sub-key the client sealed to.
//
// Caller still MUST enforce the stateful §3 checks OpenFromOwner cannot:
// version-monotonic and deliveryId-once (see seal.OpenFromOwner).
func (n *Node) OpenDelivered(sealed *seal.NodeSealed, baseAAD seal.InFlightAAD, now time.Time) ([]byte, error) {
	tried := 0
	for _, r := range n.retained {
		if r.signed.Valid(now) != nil {
			continue
		}
		tried++
		aad := baseAAD
		aad.NodeID = n.nodeID
		aad.NotAfter = r.signed.NotAfter
		pt, err := seal.OpenFromOwner(sealed, r.priv, aad, now)
		if err == nil {
			return pt, nil
		}
	}
	if tried == 0 {
		return nil, ErrNoSubKey
	}
	return nil, ErrNoMatch
}

// encodeFields produces an unambiguous length-prefixed concatenation of its
// parts (each prefixed with an 8-byte big-endian length), so distinct field
// tuples never collide into the same signed byte string. (Mirrors the encoder
// in internal/secrets/seal; kept local to avoid widening that package's API.)
func encodeFields(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += 8 + len(p)
	}
	out := make([]byte, 0, n)
	var lb [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lb[:], uint64(len(p)))
		out = append(out, lb[:]...)
		out = append(out, p...)
	}
	return out
}

func u64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}
