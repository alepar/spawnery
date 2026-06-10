package authsvc

import (
	"crypto/rand"
	"encoding/base64"
	"errors"

	"spawnery/internal/pki"
)

// ErrBadEnrollToken is returned when an enrollment token is unknown, already used, or expired.
var ErrBadEnrollToken = errors.New("authsvc: invalid or expired enrollment token")

// IssueEnrollmentToken mints a one-time, account-scoped, short-lived enrollment token. The caller is
// responsible for having authenticated that the requesting principal owns accountID (the AS is the IdP)
// — the token binds the eventual node cert to this account, so a node can never enroll for another.
func (s *Service) IssueEnrollmentToken(accountID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = enrollToken{accountID: accountID, exp: s.now().Add(s.enrollTTL)}
	s.mu.Unlock()
	return tok, nil
}

// Enroll redeems a one-time enrollment token with a node CSR and returns the issued self-hosted leaf +
// chain (PEM). The account is taken from the TOKEN (never the request); the node supplies only nodeID +
// its CSR. The token is consumed atomically before signing, so it can't be replayed.
func (s *Service) Enroll(token string, csrDER []byte, nodeID string) (certPEM, chainPEM []byte, err error) {
	s.mu.Lock()
	et, ok := s.tokens[token]
	if !ok || et.used || s.now().After(et.exp) {
		s.mu.Unlock()
		return nil, nil, ErrBadEnrollToken
	}
	et.used = true
	s.tokens[token] = et
	accountID := et.accountID
	s.mu.Unlock()

	cert, chain, err := s.intermediate.SignCSR(csrDER, nodeID, accountID, pki.ClassSelfHosted, s.now().Add(nodeCertTTL))
	if err != nil {
		return nil, nil, err
	}
	certPEM = pki.MarshalCertPEM(cert)
	for _, c := range chain {
		chainPEM = append(chainPEM, pki.MarshalCertPEM(c)...)
	}
	return certPEM, chainPEM, nil
}
