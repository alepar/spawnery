package authsvc

import (
	"crypto/ed25519"

	"spawnery/internal/sessiontoken"
)

// IssueSessionToken signs a session token with the AS session key. The caller authorizes the session
// (it is the IdP / owns the spawn->owner binding); this just attests it cryptographically so nodes can
// verify offline without trusting the CP (sp-ova design §7a).
func (s *Service) IssueSessionToken(c sessiontoken.Claims) (string, error) {
	return sessiontoken.Sign(c, s.sessionKey)
}

// SessionPubKey is the Ed25519 public key nodes pin to verify AS-signed session tokens.
func (s *Service) SessionPubKey() ed25519.PublicKey {
	return s.sessionKey.Public().(ed25519.PublicKey)
}
