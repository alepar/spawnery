package auth

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"spawnery/internal/authsvc/token"
)

// RevocationRegistry is a persistent (in-process) set of revoked token_ids and account_ids.
// Verify consults it; Apply verifies a signed feed entry and marks the relevant tokens/accounts.
type RevocationRegistry struct {
	mu            sync.RWMutex
	revokedTokens  map[string]struct{}
	revokedAccounts map[string]struct{}
	sessions      *SessionRegistry // may be nil; if set, fan-out cancels live sessions
}

// NewRevocationRegistry builds an empty registry. sessions may be nil (tests that don't need fan-out).
func NewRevocationRegistry(sessions *SessionRegistry) *RevocationRegistry {
	return &RevocationRegistry{
		revokedTokens:  make(map[string]struct{}),
		revokedAccounts: make(map[string]struct{}),
		sessions:      sessions,
	}
}

// IsRevoked reports whether the token_id or the account_id is revoked.
func (r *RevocationRegistry) IsRevoked(tokenID, accountID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if tokenID != "" {
		if _, ok := r.revokedTokens[tokenID]; ok {
			return true
		}
	}
	if accountID != "" {
		if _, ok := r.revokedAccounts[accountID]; ok {
			return true
		}
	}
	return false
}

// feedEntry is the canonical JSON shape of a revocation entry body — exactly what gets signed.
// Field order must match the AS's signRevocationEntry to produce identical bytes.
type feedEntry struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"` // JSON array of access-token token_ids
	RevokedAt int64  `json:"revoked_at"`
}

// SignedFeedEntry mirrors authsvc.SignedRevocationEntry (redeclared here to avoid importing
// internal/authsvc, which pulls the full AS service; the wire is JSON so decoupling is clean).
type SignedFeedEntry struct {
	Seq       int64  `json:"seq"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	TokenIDs  string `json:"token_ids"`
	RevokedAt int64  `json:"revoked_at"`
	Sig       string `json:"sig"` // full wire: base64url(bodyBytes)"."base64url(sig)
}

// Apply verifies the entry's sig against ks and, if valid, marks the revoked identifiers
// and fans out cancels to any attached SessionRegistry.
// Raw-bytes discipline: acts on the JSON parsed from the VERIFIED sig body bytes, not the
// unsigned top-level dup fields (WM9).
// Entry has no key_id — iterate ks (tiny set, at most 2 keys during rotation overlap).
func (r *RevocationRegistry) Apply(entry SignedFeedEntry, ks token.KeySet) error {
	var bodyBytes []byte
	var verified bool
	for _, k := range ks {
		b, err := token.VerifyArtifact(token.RevocationDomainPrefix, entry.Sig, k.Pub)
		if err == nil {
			bodyBytes = b
			verified = true
			break
		}
	}
	if !verified {
		return fmt.Errorf("revocation: signature verification failed")
	}

	// Parse the VERIFIED body bytes — not the top-level unsigned fields.
	var body feedEntry
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return fmt.Errorf("revocation: body parse: %w", err)
	}

	// Parse token_ids JSON array.
	var tokenIDs []string
	if strings.TrimSpace(body.TokenIDs) != "" && strings.TrimSpace(body.TokenIDs) != "null" {
		if err := json.Unmarshal([]byte(body.TokenIDs), &tokenIDs); err != nil {
			return fmt.Errorf("revocation: token_ids parse: %w", err)
		}
	}

	r.mu.Lock()
	for _, tid := range tokenIDs {
		r.revokedTokens[tid] = struct{}{}
	}
	if body.AccountID != "" {
		r.revokedAccounts[body.AccountID] = struct{}{}
	}
	r.mu.Unlock()

	// Fan out to SessionRegistry (outside the lock to avoid inversion).
	if r.sessions != nil {
		for _, tid := range tokenIDs {
			r.sessions.RevokeToken(tid)
		}
		if body.AccountID != "" {
			r.sessions.RevokeAccount(body.AccountID)
		}
	}
	return nil
}
