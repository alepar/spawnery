package authsvc

import (
	"crypto/ed25519"
)

// SessionPubKey is the Ed25519 public key nodes pin to verify AS-signed session tokens.
func (s *Service) SessionPubKey() ed25519.PublicKey {
	return s.sessionKey.Public().(ed25519.PublicKey)
}

// SessionPrivKey exposes the AS's Ed25519 session signing key. Used by tests and dev bootstrapping;
// production flows never call this (the AS mints tokens internally via IdP/mintAccess).
func (s *Service) SessionPrivKey() ed25519.PrivateKey { return s.sessionKey }
