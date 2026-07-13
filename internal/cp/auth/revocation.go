package auth

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

type RevocationRegistry struct {
	mu             sync.RWMutex
	revokedTokens  map[string]int64
	accountCutoffs map[string]int64
	sessions       *SessionRegistry
}

func NewRevocationRegistry(sessions *SessionRegistry) *RevocationRegistry {
	return &RevocationRegistry{
		revokedTokens: make(map[string]int64), accountCutoffs: make(map[string]int64), sessions: sessions,
	}
}

func (r *RevocationRegistry) IsRevoked(tokenID, accountID string, issuedAt int64, now time.Time) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if retainUntil, ok := r.revokedTokens[tokenID]; ok && now.Unix() < retainUntil {
		return true
	}
	cutoff, ok := r.accountCutoffs[accountID]
	return ok && issuedAt < cutoff
}

type SignedFeedEntry struct {
	Seq int64  `json:"seq"`
	Sig string `json:"sig"`
}

type SignedFeedPage struct {
	Entries []SignedFeedEntry `json:"entries"`
	HasMore bool              `json:"has_more"`
}

func (r *RevocationRegistry) ApplyPage(entries []SignedFeedEntry, artifacts *token.Verifier, now time.Time, checkpoint int64) (int64, error) {
	if r == nil || artifacts == nil {
		return checkpoint, errors.New("revocation: registry or artifact verifier is not configured")
	}
	verified := make([]*authv1.RevocationEntry, 0, len(entries))
	previous := checkpoint
	for _, entry := range entries {
		payload, err := artifacts.Verify(entry.Sig, token.ArtifactTypeRevocation, now)
		if err != nil {
			return checkpoint, fmt.Errorf("revocation: artifact verification failed: %w", err)
		}
		var body authv1.RevocationEntry
		if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, &body); err != nil {
			return checkpoint, fmt.Errorf("revocation: body parse: %w", err)
		}
		if len(body.ProtoReflect().GetUnknown()) != 0 {
			return checkpoint, errors.New("revocation: body has unknown fields")
		}
		canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&body)
		if err != nil || !bytes.Equal(canonical, payload) {
			return checkpoint, errors.New("revocation: body is not deterministic protobuf")
		}
		if body.Seq != entry.Seq {
			return checkpoint, fmt.Errorf("revocation: verified sequence %d does not match feed sequence %d", body.Seq, entry.Seq)
		}
		if err := validateRevocationEntry(&body, previous); err != nil {
			return checkpoint, err
		}
		previous = body.Seq
		verified = append(verified, &body)
	}

	nowUnix := now.Unix()
	fanout := make(map[string]struct{})
	r.mu.Lock()
	for tokenID, retainUntil := range r.revokedTokens {
		if retainUntil <= nowUnix {
			delete(r.revokedTokens, tokenID)
		}
	}
	for _, body := range verified {
		for _, revoked := range body.RevokedTokens {
			if revoked.RetainUntil <= nowUnix {
				continue
			}
			if revoked.RetainUntil > r.revokedTokens[revoked.TokenId] {
				r.revokedTokens[revoked.TokenId] = revoked.RetainUntil
			}
			fanout[revoked.TokenId] = struct{}{}
		}
		if body.RevokeTokensIssuedBefore > r.accountCutoffs[body.AccountId] {
			r.accountCutoffs[body.AccountId] = body.RevokeTokensIssuedBefore
		}
	}
	r.mu.Unlock()

	if r.sessions != nil {
		for tokenID := range fanout {
			r.sessions.RevokeToken(tokenID)
		}
	}
	return previous, nil
}

func validateRevocationEntry(body *authv1.RevocationEntry, previous int64) error {
	if body.Seq <= previous || body.AccountId == "" || body.RevokedAt <= 0 {
		return errors.New("revocation: invalid sequence or identity")
	}
	if body.FamilyId == "" {
		if body.RevokeTokensIssuedBefore <= 0 {
			return errors.New("revocation: account cutoff required")
		}
	} else if body.RevokeTokensIssuedBefore != 0 || len(body.RevokedTokens) == 0 {
		return errors.New("revocation: invalid family entry")
	}
	seen := make(map[string]struct{}, len(body.RevokedTokens))
	for _, revoked := range body.RevokedTokens {
		if revoked == nil || revoked.TokenId == "" || revoked.RetainUntil <= body.RevokedAt {
			return errors.New("revocation: invalid revoked token")
		}
		if _, ok := seen[revoked.TokenId]; ok {
			return errors.New("revocation: duplicate revoked token")
		}
		seen[revoked.TokenId] = struct{}{}
	}
	return nil
}
