package authsvc

// PoP (Proof-of-Possession) verification for /refresh [AM5].
//
// The client signs the message:
//   domain || sha256(refresh_token_string_bytes) || big-endian int64(timestamp) || nonce(≥16B)
// where domain = refreshPoPDomain = "spawnery/refresh-pop/v1".
// Signature is ECDSA P-256 over SHA-256(message), **P1363 raw 64-byte r||s encoding**.
//
// Cross-language contract (frozen — A3/spawnctl and A5/SPA must match):
//   message   = []byte(domain) || sha256(refresh_token_bytes) || be64(timestamp) || nonce
//   hash      = SHA-256(message)
//   sig format = P1363 raw 64-byte r||s (WebCrypto SubtleCrypto.sign("ECDSA") default)
//
// DER (ASN.1) sig encoding is NOT used because WebCrypto produces raw P1363 by default.
// Reject stale/future timestamp (±90s skew). Replay via nonce is bounded by token rotation
// (each refresh mints a new token, so a replayed PoP on the consumed token gains nothing
// beyond the grace window).

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"math/big"
	"time"
)

// PoPProof carries the three fields a client includes in the Authorization header or request
// body of a /refresh call.
type PoPProof struct {
	// RefreshTokenHash is SHA-256 over the raw refresh-token string bytes — the same value
	// used as the DB row key. The client SHA-256s the token it is presenting.
	RefreshTokenHash []byte
	// Timestamp is Unix seconds at signature time.
	Timestamp int64
	// Nonce is a random byte string (≥16 bytes).
	Nonce []byte
	// Sig is the ECDSA P-256 P1363 raw r||s signature (64 bytes) over SHA-256(message).
	Sig []byte
}

var (
	ErrPoPMissing   = errors.New("pop: proof-of-possession required")
	ErrPoPStale     = errors.New("pop: timestamp out of window")
	ErrPoPBadSig    = errors.New("pop: signature invalid")
	ErrPoPBadNonce  = errors.New("pop: nonce too short")
	ErrPoPBadKey    = errors.New("pop: session key not P-256")
)

// VerifyPoP checks the client's session-key PoP against the stored DER SPKI material [AM5].
// spkiDER is raw bytes as stored in refresh_sessions.session_pubkey_spki.
func VerifyPoP(spkiDER []byte, proof PoPProof, now time.Time) error {
	if proof.Sig == nil {
		return ErrPoPMissing
	}
	if len(proof.Nonce) < 16 {
		return ErrPoPBadNonce
	}
	// Check timestamp skew.
	ts := time.Unix(proof.Timestamp, 0)
	if now.Sub(ts).Abs() > popSkew {
		return ErrPoPStale
	}
	// Parse the stored public key.
	pub, err := x509.ParsePKIXPublicKey(spkiDER)
	if err != nil {
		return ErrPoPBadKey
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return ErrPoPBadKey
	}

	// Build signed message: domain || sha256(refresh_token_hash) || be64(timestamp) || nonce.
	// Note: proof.RefreshTokenHash is already SHA-256 of the token string bytes — we include it
	// directly as the pre-hashed binding (prevents cross-purpose reuse of this proof).
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(proof.Timestamp))
	msg := make([]byte, 0, len(refreshPoPDomain)+len(proof.RefreshTokenHash)+8+len(proof.Nonce))
	msg = append(msg, refreshPoPDomain...)
	msg = append(msg, proof.RefreshTokenHash...)
	msg = append(msg, tsBytes[:]...)
	msg = append(msg, proof.Nonce...)
	digest := sha256.Sum256(msg)

	// Sig is P1363 raw r||s (64 bytes).
	if len(proof.Sig) != 64 {
		return ErrPoPBadSig
	}
	r := new(big.Int).SetBytes(proof.Sig[:32])
	s := new(big.Int).SetBytes(proof.Sig[32:])
	if !ecdsa.Verify(ec, digest[:], r, s) {
		return ErrPoPBadSig
	}
	return nil
}
