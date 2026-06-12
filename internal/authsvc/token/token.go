// Package token mints and verifies AS-signed artifacts: session access tokens and revocation
// feed entries (auth-identity design §3 [MC1][AM4]). It is a deliberate leaf package — A2's CP
// verifier imports it without pulling in the AS service — so it depends only on gen/auth/v1,
// protobuf, and stdlib crypto.
package token

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

// Domain-separation prefixes: every AS-signed artifact class carries a mandatory prefix so a
// signature over one class can never validate as another [MC1].
const (
	DomainPrefix           = "spawnery/session-token/v1"
	RevocationDomainPrefix = "spawnery/revocation/v1"
)

// Errors are sentinel so verifiers can map them to machine-readable codes.
var (
	ErrMalformed  = errors.New("token: malformed wire format")
	ErrSignature  = errors.New("token: signature verification failed")
	ErrUnknownKey = errors.New("token: unknown key_id")
	ErrExpired    = errors.New("token: expired")
	ErrNotYet     = errors.New("token: issued in the future")
)

// allowed clock skew on issued_at (a token "issued" further in the future than this is refused).
const issuedAtSkew = 60 * time.Second

// Mint signs body with priv and returns the wire form:
// base64url(body) "." base64url(ed25519(DomainPrefix || body)) — RawURLEncoding, unpadded [MC1].
func Mint(body *authv1.SessionTokenBody, priv ed25519.PrivateKey) (string, error) {
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return "", err
	}
	return SignArtifact(DomainPrefix, bodyBytes, priv), nil
}

// SignArtifact signs raw body bytes under a domain prefix and returns the two-part wire string.
// Shared by session tokens and revocation entries (same key, distinct domains [MC1]).
func SignArtifact(domain string, bodyBytes []byte, priv ed25519.PrivateKey) string {
	msg := make([]byte, 0, len(domain)+len(bodyBytes))
	msg = append(msg, domain...)
	msg = append(msg, bodyBytes...)
	sig := ed25519.Sign(priv, msg)
	return base64.RawURLEncoding.EncodeToString(bodyBytes) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyArtifact splits wire, verifies sig over (domain || EXACT received body bytes) against
// pub, and returns the body bytes. Verification always runs over the raw received bytes —
// never a re-serialization (WM9 discipline).
func VerifyArtifact(domain, wire string, pub ed25519.PublicKey) ([]byte, error) {
	bodyB64, sigB64, ok := strings.Cut(wire, ".")
	if !ok {
		return nil, ErrMalformed
	}
	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, ErrMalformed
	}
	msg := make([]byte, 0, len(domain)+len(bodyBytes))
	msg = append(msg, domain...)
	msg = append(msg, bodyBytes...)
	if !ed25519.Verify(pub, msg, sig) {
		return nil, ErrSignature
	}
	return bodyBytes, nil
}

// Verify checks an access token against the key set and clock, returning the parsed body.
// Key selection: the body's key_id picks the verification key, but the signature is still
// checked over the exact received body bytes — a forged key_id only selects a key that will
// then fail verification. Audience checking is the CALLER's job (the CP enforces aud=="cp",
// nodes aud=="node" [MC2]); the body is returned as-is.
func Verify(wire string, ks KeySet, now time.Time) (*authv1.SessionTokenBody, error) {
	bodyB64, _, ok := strings.Cut(wire, ".")
	if !ok {
		return nil, ErrMalformed
	}
	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return nil, ErrMalformed
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(bodyBytes, &body); err != nil {
		return nil, ErrMalformed
	}
	pub, ok := ks.Lookup(body.KeyId)
	if !ok {
		return nil, ErrUnknownKey
	}
	if _, err := VerifyArtifact(DomainPrefix, wire, pub); err != nil {
		return nil, err
	}
	if !now.Before(time.Unix(body.ExpiresAt, 0)) {
		return nil, ErrExpired
	}
	if time.Unix(body.IssuedAt, 0).After(now.Add(issuedAtSkew)) {
		return nil, ErrNotYet
	}
	return &body, nil
}

// SessionKeyHash is SHA-256 over the DER SPKI exactly as x509.MarshalPKIXPublicKey /
// WebCrypto exportKey('spki') emit [AM11]. Shared by the mint path (cnf claim) and the
// refresh-PoP path.
func SessionKeyHash(spkiDER []byte) []byte {
	sum := sha256.Sum256(spkiDER)
	return sum[:]
}

// KeyID derives the key selector from a public key: the first 16 hex chars of SHA-256 over
// the DER SPKI. Derived, not configured — config cannot desync an id from its key (mirrors
// pki.PublicKeyFingerprint discipline) [AM4].
func KeyID(pub ed25519.PublicKey) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:8]), nil
}

// LoadSigningKey parses a PKCS#8 Ed25519 private key PEM and returns the key plus its derived
// key_id. Production loads this from disk (0600, escrowed — see deploy/authsvc); generating
// in memory is dev-only [AM13].
func LoadSigningKey(pemBytes []byte) (ed25519.PrivateKey, string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, "", errors.New("token: no PEM block in signing key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("token: parse signing key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, "", errors.New("token: signing key is not Ed25519")
	}
	id, err := KeyID(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, "", err
	}
	return priv, id, nil
}

// MarshalSigningKeyPEM emits the PKCS#8 PEM form LoadSigningKey reads (dev bootstrap, tests).
func MarshalSigningKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePublicKeyPEM parses an Ed25519 public key PEM (PKIX) — rotation "next" keys [AM4].
func ParsePublicKeyPEM(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("token: no PEM block in public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("token: parse public key: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("token: public key is not Ed25519")
	}
	return pub, nil
}

// MintNode mints a short-lived aud=node access token bound to spkiDER (the cnf claim) [A4][AM12].
// priv is the Ed25519 signing key; keyID must equal KeyID(priv.Public().(ed25519.PublicKey)).
// Used by the dev CP to issue cnf-bearing node tokens so the full A4 verification chain runs in
// `just dev`. A 15-minute TTL mirrors the aud=cp access token.
func MintNode(priv ed25519.PrivateKey, keyID, accountID string, spkiDER []byte, now time.Time) (string, error) {
	var tidBytes [16]byte
	if _, err := rand.Read(tidBytes[:]); err != nil {
		return "", fmt.Errorf("MintNode: random token_id: %w", err)
	}
	const ttl = 15 * time.Minute
	body := &authv1.SessionTokenBody{
		AccountId:      accountID,
		TokenId:        hex.EncodeToString(tidBytes[:]),
		Audience:       "node",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(ttl).Unix(),
		SessionKeyHash: SessionKeyHash(spkiDER),
		KeyId:          keyID,
	}
	return Mint(body, priv)
}

// equalPub avoids subtle nil-vs-empty issues when comparing keys in KeySet construction.
func equalPub(a, b ed25519.PublicKey) bool { return bytes.Equal(a, b) }
