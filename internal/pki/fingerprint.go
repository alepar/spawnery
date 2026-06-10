package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
)

// PublicKeyFingerprint returns a stable, lowercase-hex SHA-256 fingerprint over the SubjectPublicKeyInfo
// (SPKI) DER encoding of pub. It identifies a keypair independently of any CSR signature or certificate,
// so a node, the owner's client, and the AS all derive the SAME value from the same public key. This is
// the binding handle for fingerprint-bound enrollment tokens (owner-sealed-secrets design §5): the owner
// scopes a token to this fingerprint, and the AS recomputes it from the redeemed CSR's public key.
func PublicKeyFingerprint(pub crypto.PublicKey) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("pki: marshal public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

// CSRPublicKeyFingerprint parses a DER CSR and returns the SPKI fingerprint of the public key it carries.
// The AS uses this at redemption to check the CSR's key matches the token's bound fingerprint — so a
// leaked or CP-relayed token cannot be redeemed with a substituted key (the CP-can't-substitute property).
func CSRPublicKeyFingerprint(csrDER []byte) (string, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return "", fmt.Errorf("pki: parse csr: %w", err)
	}
	return PublicKeyFingerprint(csr.PublicKey)
}

// NewNodeKey generates a fresh node P-256 keypair. The node persists this and presents CSRs over it; its
// SPKI fingerprint (PublicKeyFingerprint) is what an enrollment token is bound to.
func NewNodeKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// NodeCSRForKey builds a CSR over an existing node key (proving possession). The CSR carries no
// authoritative names — the CA assigns the SAN at signing time (SignCSR).
func NodeCSRForKey(key *ecdsa.PrivateKey) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
}
