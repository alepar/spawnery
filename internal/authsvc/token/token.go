// Package token signs and verifies purpose-constrained, root-anchored authorization artifacts.
package token

import (
	"crypto/sha256"
	"errors"
	"time"

	authv1 "spawnery/gen/auth/v1"
)

const (
	DomainPrefix           = "spawnery/session-token/v1"
	RevocationDomainPrefix = "spawnery/revocation/v1"
)

// AudienceCP identifies session artifacts intended for control-plane authorization.
const AudienceCP = "cp"

// Errors are sentinel so verifiers can map them to machine-readable codes.
var (
	ErrMalformed  = errors.New("token: malformed wire format")
	ErrSignature  = errors.New("token: signature verification failed")
	ErrUnknownKey = errors.New("token: unknown key_id")
	ErrExpired    = errors.New("token: expired")
	ErrNotYet     = errors.New("token: issued in the future")
)

const issuedAtSkew = 60 * time.Second

// ValidateSessionBody enforces clock semantics after an envelope's exact payload is authenticated.
func ValidateSessionBody(body *authv1.SessionTokenBody, now time.Time) error {
	if body == nil {
		return ErrMalformed
	}
	if !now.Before(time.Unix(body.ExpiresAt, 0)) {
		return ErrExpired
	}
	if time.Unix(body.IssuedAt, 0).After(now.Add(issuedAtSkew)) {
		return ErrNotYet
	}
	return nil
}

// SessionKeyHash is SHA-256 over the DER SPKI used by the session-token cnf claim.
func SessionKeyHash(spkiDER []byte) []byte {
	sum := sha256.Sum256(spkiDER)
	return sum[:]
}
