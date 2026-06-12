// Package seal implements the owner-sealed-secrets cryptographic primitives:
// an HPKE-based per-recipient envelope (a fresh DEK per write, sealed to each
// enrolled device), deterministic device keypairs derived from a BIP-39 seed, a
// member-signed hash-chained device-set registry, and the re-seal-to-node
// delivery leg.
//
// This package is the PURE-CRYPTO half of the design
// (docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md). It does
// NOT verify node certificates against pinned roots — node HPKE pubkeys are
// taken as already-trusted input. That delivery/PKI leg is a separate slice.
//
// Suite: DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + AES-256-GCM, HPKE Base mode.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cloudflare/circl/hpke"
	"github.com/cloudflare/circl/kem"
)

// suite is the single HPKE ciphersuite used everywhere (decision S.2: HPKE
// everywhere, one primitive).
var suite = hpke.NewSuite(
	hpke.KEM_X25519_HKDF_SHA256,
	hpke.KDF_HKDF_SHA256,
	hpke.AEAD_AES256GCM,
)

// kemScheme is the X25519 KEM scheme; its key marshaling is the raw 32-byte
// X25519 form, compatible with stdlib crypto/ecdh.
var kemScheme = hpke.KEM_X25519_HKDF_SHA256.Scheme()

const (
	// dekSize is the AES-256-GCM key length.
	dekSize = 32
	// x25519KeySize is the raw X25519 public/private key length.
	x25519KeySize = 32

	// infoAtRest domain-separates the at-rest DEK seal from the in-flight
	// payload seal so a ciphertext from one context cannot be opened in the
	// other.
	infoAtRest = "spawnery/secrets/seal/at-rest/v1"
	// infoInFlight domain-separates the delivery (node) seal.
	infoInFlight = "spawnery/secrets/seal/in-flight/v1"
)

// Errors returned by this package.
var (
	ErrNoRecipient  = errors.New("seal: no recipients")
	ErrBadKeySize   = errors.New("seal: malformed key (wrong length)")
	ErrNotRecipient = errors.New("seal: device key is not a recipient of this envelope")
	ErrTampered     = errors.New("seal: envelope failed authentication (tampered or spliced)")
	ErrAADMismatch  = errors.New("seal: AAD mismatch (wrong context or tampered)")
	ErrExpired      = errors.New("seal: delivery expired (notAfter < now)")
	ErrMalformed    = errors.New("seal: malformed sealed object")
)

// X25519PubKey is a raw 32-byte X25519 (HPKE) public key.
type X25519PubKey []byte

// AtRestAAD is the additional authenticated data bound into every at-rest DEK
// seal: (accountId, secretId, version). It stops a compromised CP from splicing
// seals across envelopes or replaying an old version as current (§2).
// json tags use snake_case to match the TypeScript AtRestAAD interface.
type AtRestAAD struct {
	AccountID string `json:"account_id"`
	SecretID  string `json:"secret_id"`
	Version   uint64 `json:"version"`
}

func (a AtRestAAD) bytes() []byte {
	return encodeFields(
		[]byte("at-rest/v1"),
		[]byte(a.AccountID),
		[]byte(a.SecretID),
		u64(a.Version),
	)
}

// RecipientSeal is the HPKE-Base seal of the DEK to one recipient device pubkey
// (the age-stanza pattern).
type RecipientSeal struct {
	// Recipient is the 32-byte X25519 pubkey this stanza was sealed to. It is a
	// hint for trial-open; it is not trusted (the seal itself authenticates).
	Recipient X25519PubKey `json:"recipient"`
	// Enc is the HPKE encapsulated key.
	Enc []byte `json:"enc"`
	// CT is the sealed DEK.
	CT []byte `json:"ct"`
}

// Envelope is the opaque, CP-stored object: payload encrypted under a fresh
// random DEK, with the DEK HPKE-sealed to each recipient (§2). The AtRest fields
// are stored in the clear but are bound into every seal as AAD, so any tamper
// (e.g. a version downgrade) breaks authentication on Open.
type Envelope struct {
	AtRest     AtRestAAD       `json:"at_rest"`
	Recipients []RecipientSeal `json:"recipients"`
	// Nonce is the AES-256-GCM nonce for the payload ciphertext.
	Nonce []byte `json:"nonce"`
	// CT is the payload ciphertext under the DEK.
	CT []byte `json:"ct"`
}

// Seal encrypts payload under a FRESH random DEK (a new DEK on every call —
// roast M2) and HPKE-Base-seals that DEK to each recipient pubkey, binding
// aadAtRest into every seal.
func Seal(payload []byte, recipients []X25519PubKey, aadAtRest AtRestAAD) (*Envelope, error) {
	if len(recipients) == 0 {
		return nil, ErrNoRecipient
	}
	aad := aadAtRest.bytes()

	// Fresh DEK per write.
	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("seal: read DEK: %w", err)
	}

	// Encrypt the payload under the DEK (AES-256-GCM), binding aadAtRest.
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("seal: read nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, payload, aad)

	// HPKE-Base-seal the DEK to each recipient.
	seals := make([]RecipientSeal, 0, len(recipients))
	for _, r := range recipients {
		pk, err := parsePub(r)
		if err != nil {
			return nil, err
		}
		sender, err := suite.NewSender(pk, []byte(infoAtRest))
		if err != nil {
			return nil, fmt.Errorf("seal: new sender: %w", err)
		}
		enc, sealer, err := sender.Setup(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("seal: hpke setup: %w", err)
		}
		dekCT, err := sealer.Seal(dek, aad)
		if err != nil {
			return nil, fmt.Errorf("seal: hpke seal: %w", err)
		}
		seals = append(seals, RecipientSeal{
			Recipient: append(X25519PubKey(nil), r...),
			Enc:       enc,
			CT:        dekCT,
		})
	}

	return &Envelope{
		AtRest:     aadAtRest,
		Recipients: seals,
		Nonce:      nonce,
		CT:         ct,
	}, nil
}

// Open recovers the payload from an envelope using one device's X25519 private
// key. It tries each recipient stanza until one unseals (a rotation- and
// hint-tamper-robust trial open), then AEAD-decrypts the payload.
func Open(env *Envelope, deviceX25519Priv []byte) ([]byte, error) {
	dek, err := openDEK(env, deviceX25519Priv)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(env.Nonce) != gcm.NonceSize() {
		return nil, ErrMalformed
	}
	pt, err := gcm.Open(nil, env.Nonce, env.CT, env.AtRest.bytes())
	if err != nil {
		return nil, ErrTampered
	}
	return pt, nil
}

// openDEK recovers the DEK for a device by trial-opening each recipient stanza.
// Used by Open and by ReSealToNode. The aadAtRest is recomputed from the
// envelope's own fields, so a CP that rewrites those fields breaks the seal.
func openDEK(env *Envelope, deviceX25519Priv []byte) ([]byte, error) {
	sk, err := parsePriv(deviceX25519Priv)
	if err != nil {
		return nil, err
	}
	aad := env.AtRest.bytes()
	for i := range env.Recipients {
		rs := env.Recipients[i]
		recv, err := suite.NewReceiver(sk, []byte(infoAtRest))
		if err != nil {
			return nil, fmt.Errorf("seal: new receiver: %w", err)
		}
		opener, err := recv.Setup(rs.Enc)
		if err != nil {
			continue // wrong stanza / malformed enc for this key
		}
		dek, err := opener.Open(rs.CT, aad)
		if err != nil {
			continue // not our stanza, or tampered
		}
		if len(dek) != dekSize {
			return nil, ErrMalformed
		}
		return dek, nil
	}
	return nil, ErrNotRecipient
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != dekSize {
		return nil, ErrBadKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func parsePub(b []byte) (kem.PublicKey, error) {
	if len(b) != x25519KeySize {
		return nil, ErrBadKeySize
	}
	pk, err := kemScheme.UnmarshalBinaryPublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("seal: parse pubkey: %w", err)
	}
	return pk, nil
}

func parsePriv(b []byte) (kem.PrivateKey, error) {
	if len(b) != x25519KeySize {
		return nil, ErrBadKeySize
	}
	sk, err := kemScheme.UnmarshalBinaryPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("seal: parse privkey: %w", err)
	}
	return sk, nil
}

// encodeFields produces an unambiguous length-prefixed concatenation of its
// parts (each prefixed with an 8-byte big-endian length), so distinct field
// tuples never collide into the same AAD byte string.
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
