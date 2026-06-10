package seal

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Delivery leg (§3): the owner's client unseals the DEK with a device key,
// re-seals the PAYLOAD to the target node's HPKE sub-key under an in-flight AAD,
// and the CP relays the ciphertext. The node Open enforces AAD equality and the
// notAfter clock check. A ciphertext is useless to any other node (different KEM
// key) or context (different AAD).
//
// NOTE: the node HPKE pubkey is taken as ALREADY-TRUSTED input here. Verifying
// it against the node cert chain + pinned roots + the AS revocation list is the
// delivery/PKI leg, which is OUT OF SCOPE for this pure-crypto package.

// InFlightAAD is the per-delivery context bound into the node seal (§3, roast
// M11): (spawnId, generation, nodeId, notAfter, version, deliveryId).
type InFlightAAD struct {
	SpawnID    string
	Generation uint64
	NodeID     string
	NotAfter   time.Time
	Version    uint64
	DeliveryID string // node-issued one-time nonce
}

func (a InFlightAAD) bytes() []byte {
	return encodeFields(
		[]byte("in-flight/v1"),
		[]byte(a.SpawnID),
		u64(a.Generation),
		[]byte(a.NodeID),
		u64(uint64(a.NotAfter.UnixNano())),
		u64(a.Version),
		[]byte(a.DeliveryID),
	)
}

// NodeSealed is the HPKE-Base seal of a secret payload to one node sub-key.
type NodeSealed struct {
	Enc []byte `json:"enc"` // HPKE encapsulated key
	CT  []byte `json:"ct"`  // sealed payload
}

// ReSealToNode unseals the envelope's DEK with the device key, recovers the
// payload, and re-seals that PAYLOAD to nodeHPKEPub via HPKE Base with the given
// in-flight AAD. The node pubkey is trusted input (see package note).
func ReSealToNode(env *Envelope, deviceX25519Priv []byte, nodeHPKEPub []byte, aad InFlightAAD) (*NodeSealed, error) {
	payload, err := Open(env, deviceX25519Priv)
	if err != nil {
		return nil, err
	}
	pk, err := parsePub(nodeHPKEPub)
	if err != nil {
		return nil, err
	}
	sender, err := suite.NewSender(pk, []byte(infoInFlight))
	if err != nil {
		return nil, fmt.Errorf("seal: new node sender: %w", err)
	}
	enc, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("seal: node hpke setup: %w", err)
	}
	ct, err := sealer.Seal(payload, aad.bytes())
	if err != nil {
		return nil, fmt.Errorf("seal: node hpke seal: %w", err)
	}
	return &NodeSealed{Enc: enc, CT: ct}, nil
}

// OpenFromOwner is the node side: it enforces notAfter ≥ now (a clock check
// HPKE does not do) and AAD equality (any mismatch in the expected context
// breaks the HPKE Open), then returns the payload.
//
// The remaining two §3 enforcements need state this pure-crypto function does
// not hold and are left as documented hooks for the caller:
//   - version-monotonic: reject a version older than the highest seen for the
//     secret (defeats a CP replaying a pre-rotation ciphertext within the
//     sub-key window).
//   - deliveryId-once: accept each one-time deliveryId exactly once (defeats
//     same-context replay, which AAD alone cannot).
//
// The caller MUST apply both against its own per-secret high-water mark and
// seen-deliveryId set before trusting the returned payload.
func OpenFromOwner(sealed *NodeSealed, nodeHPKEPriv []byte, expectedAAD InFlightAAD, now time.Time) ([]byte, error) {
	if sealed == nil {
		return nil, ErrMalformed
	}
	// Clock check first — HPKE does not check expiry.
	if expectedAAD.NotAfter.Before(now) {
		return nil, ErrExpired
	}
	sk, err := parsePriv(nodeHPKEPriv)
	if err != nil {
		return nil, err
	}
	recv, err := suite.NewReceiver(sk, []byte(infoInFlight))
	if err != nil {
		return nil, fmt.Errorf("seal: new node receiver: %w", err)
	}
	opener, err := recv.Setup(sealed.Enc)
	if err != nil {
		return nil, ErrMalformed
	}
	payload, err := opener.Open(sealed.CT, expectedAAD.bytes())
	if err != nil {
		// Wrong context (AAD mismatch), wrong node key, or tampered ciphertext.
		return nil, ErrAADMismatch
	}
	return payload, nil
}

// NodeKeyPair is a convenience helper to generate a node HPKE sub-keypair for
// tests / callers. The signing/publishing of this pubkey under the node cert key
// is the out-of-scope delivery leg.
func NodeKeyPair() (pub, priv []byte, err error) {
	pk, sk, err := kemScheme.GenerateKeyPair()
	if err != nil {
		return nil, nil, fmt.Errorf("seal: gen node keypair: %w", err)
	}
	pub, err = pk.MarshalBinary()
	if err != nil {
		return nil, nil, err
	}
	priv, err = sk.MarshalBinary()
	if err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}
