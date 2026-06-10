package authsvc

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"spawnery/internal/pki"
)

// ErrBadEnrollToken is returned when an enrollment token is unknown, already used, or expired.
var ErrBadEnrollToken = errors.New("authsvc: invalid or expired enrollment token")

// ErrTokenFingerprintMismatch is returned when a fingerprint-bound token is redeemed with a CSR whose
// public-key fingerprint differs from the one the token was bound to. This is the core hardening: a
// leaked or CP-relayed token cannot be redeemed with a substituted key (owner-sealed-secrets §5).
var ErrTokenFingerprintMismatch = errors.New("authsvc: CSR key does not match the token's bound fingerprint")

// ErrUnsignableClass is returned when a token is requested for a class the AS cannot sign. The AS holds
// only the name-constrained self-hosted intermediate (node-auth §4); it can never issue a cloud identity,
// so binding a token to class=cloud is rejected at issuance — class scoping prevents escalation.
var ErrUnsignableClass = errors.New("authsvc: AS cannot issue this class (only self-hosted)")

func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssueBoundEnrollmentToken mints a one-time enrollment token scoped to
// (accountID, class, node-pubkey-fingerprint, expiry). The owner's client obtains the node's CSR
// public-key fingerprint (pki.PublicKeyFingerprint over the node's SPKI) out of band and requests a
// token bound to it over the direct, pinned AS connection. At redemption the AS verifies the CSR's key
// matches this fingerprint (Enroll), so a token observed/relayed by the CP cannot be redeemed with a
// substituted key — the ACME external-account-binding property. Single-use makes interception
// detectable; the TTL bounds exposure; account/class scoping prevents cross-account/class escalation.
//
// The caller is responsible for having authenticated that the requesting principal owns accountID (the
// AS is the IdP). class must be one the AS can sign (self-hosted); fingerprint must be non-empty.
func (s *Service) IssueBoundEnrollmentToken(accountID, class, fingerprint string) (string, error) {
	if class != pki.ClassSelfHosted {
		return "", fmt.Errorf("%w: %q", ErrUnsignableClass, class)
	}
	if fingerprint == "" {
		return "", errors.New("authsvc: bound enrollment token requires a non-empty fingerprint")
	}
	tok, err := randToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[tok] = enrollToken{accountID: accountID, class: class, fingerprint: fingerprint, exp: s.now().Add(s.enrollTTL)}
	s.mu.Unlock()
	return tok, nil
}

// IssueEnrollmentToken mints a LEGACY, UNBOUND one-time enrollment token (account-scoped, short-lived,
// single-use). It carries no key fingerprint, so any well-formed CSR can redeem it — a token an attacker
// observes can be redeemed with the attacker's own key. Retained for backward compatibility; production
// callers SHOULD use IssueBoundEnrollmentToken so the redeeming key is pinned at issuance.
func (s *Service) IssueEnrollmentToken(accountID string) (string, error) {
	tok, err := randToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.tokens[tok] = enrollToken{accountID: accountID, class: pki.ClassSelfHosted, exp: s.now().Add(s.enrollTTL)}
	s.mu.Unlock()
	return tok, nil
}

// Enroll redeems a one-time enrollment token with a node CSR and returns the issued leaf + chain (PEM).
// The account AND class are taken from the TOKEN (never the request); the node supplies only nodeID + its
// CSR. For a fingerprint-bound token (IssueBoundEnrollmentToken) the AS additionally verifies the CSR's
// public-key fingerprint equals the bound one and REJECTS a mismatch WITHOUT consuming the token (so a
// thief's failed attempt cannot burn the legitimate node's token). On a valid redemption the token is
// consumed atomically before signing, so it can't be replayed.
func (s *Service) Enroll(token string, csrDER []byte, nodeID string) (certPEM, chainPEM []byte, err error) {
	// Compute the CSR fingerprint outside the lock (pure CPU); compared against the token below.
	csrFP, fpErr := pki.CSRPublicKeyFingerprint(csrDER)

	s.mu.Lock()
	et, ok := s.tokens[token]
	if !ok || et.used || s.now().After(et.exp) {
		s.mu.Unlock()
		return nil, nil, ErrBadEnrollToken
	}
	if et.fingerprint != "" {
		if fpErr != nil {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("%w: %v", ErrTokenFingerprintMismatch, fpErr)
		}
		// Constant-time-ish equality is unnecessary (hex of a public value), plain compare is fine.
		if csrFP != et.fingerprint {
			s.mu.Unlock() // do NOT consume — leave the token usable for the legitimate node.
			return nil, nil, ErrTokenFingerprintMismatch
		}
	}
	et.used = true
	s.tokens[token] = et
	accountID, class := et.accountID, et.class
	s.mu.Unlock()

	cert, chain, err := s.intermediate.SignCSR(csrDER, nodeID, accountID, class, s.now().Add(nodeCertTTL))
	if err != nil {
		return nil, nil, err
	}
	certPEM = pki.MarshalCertPEM(cert)
	for _, c := range chain {
		chainPEM = append(chainPEM, pki.MarshalCertPEM(c)...)
	}
	return certPEM, chainPEM, nil
}
