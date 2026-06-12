package authsvc

// Refresh-family logic: PoP verification, grace-window idempotency, rotation, revocation.
// Called from both the HTTP /refresh endpoint (web) and the device-grant token endpoint (CLI).
// All DB mutations run inside WithTx for atomicity [AM3, AM5, AM6].

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"spawnery/internal/authsvc/store"
)

// successorPair is what gets stored in successor_cache and returned on idempotent replay [AM3].
type successorPair struct {
	AccessToken  string `json:"a"` // the minted access wire token
	RefreshToken string `json:"r"` // the raw refresh token (before next supersede)
}

// ErrFamilyRevoked is returned when a replay attack is detected (reuse outside grace or ≥2 gen).
var ErrFamilyRevoked = errors.New("refresh: family revoked due to token reuse")

// rotateSession mints a successor access+refresh pair, stamps the predecessor, and slides the
// window. Returns (accessWire, rawRefresh). Must be called with the tx store so all reads and
// writes use the same transaction connection [R5].
func (i *IdP) rotateSession(ctx context.Context, tx store.Store, row store.RefreshSession, now time.Time) (
	accessWire, rawRefresh string, err error,
) {
	u, err := tx.Users().GetByID(ctx, row.AccountID)
	if err != nil {
		return "", "", fmt.Errorf("refresh: load user: %w", err)
	}

	accessWire, _, err = i.mintAccess(u, row.SessionPubkeySPKI, now)
	if err != nil {
		return "", "", fmt.Errorf("refresh: mint access: %w", err)
	}

	rawRefresh = randOpaque()
	newHash := sha256Hex(rawRefresh)
	tokenID := uuid.NewString()

	successor := store.RefreshSession{
		TokenHash:         newHash,
		AccountID:         row.AccountID,
		FamilyID:          row.FamilyID,
		ClientKind:        row.ClientKind,
		SessionPubkeySPKI: row.SessionPubkeySPKI,
		AccessTokenID:     tokenID,
		CreatedAt:         now.Unix(),
		LastUsedAt:        now.Unix(),
		ExpiresAt:         now.Add(refreshSliding).Unix(),
		FamilyCreatedAt:   row.FamilyCreatedAt,
	}

	cache, err := json.Marshal(successorPair{AccessToken: accessWire, RefreshToken: rawRefresh})
	if err != nil {
		return "", "", err
	}

	if err := tx.RefreshSessions().Supersede(ctx, row.TokenHash, successor, string(cache), now.Unix()); err != nil {
		return "", "", fmt.Errorf("refresh: supersede: %w", err)
	}
	return accessWire, rawRefresh, nil
}

// handleRefresh processes a /refresh request: PoP check, grace, rotation.
// Returns (accessWire, rawRefresh, error). The caller sets the cookie.
func (i *IdP) handleRefresh(ctx context.Context, rawToken string, proof PoPProof, now time.Time) (
	accessWire, rawRefresh string, err error,
) {
	tokenHash := sha256Hex(rawToken)

	var outAccess, outRefresh string
	err = i.store.WithTx(ctx, func(tx store.Store) error {
		row, err := tx.RefreshSessions().Get(ctx, tokenHash)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("refresh: token not found")
		}
		if err != nil {
			return err
		}
		if row.Revoked {
			return ErrFamilyRevoked
		}
		// 90-day family max-age [AM6].
		if now.Unix() >= row.FamilyCreatedAt+int64(familyMaxAge.Seconds()) {
			return fmt.Errorf("refresh: family max age exceeded")
		}
		// 30-day sliding expiry.
		if now.Unix() >= row.ExpiresAt {
			return fmt.Errorf("refresh: token expired")
		}

		// PoP must pass before we do anything else [AM5].
		refreshHash := sha256sum([]byte(rawToken))
		proof.RefreshTokenHash = refreshHash
		if err := VerifyPoP(row.SessionPubkeySPKI, proof, now); err != nil {
			return fmt.Errorf("refresh: %w", err)
		}

		// Already superseded — check grace window [AM3].
		if row.SupersededBy != "" {
			// If within grace AND has a cached successor pair, return idempotent result.
			age := now.Unix() - row.SupersededAt
			if age <= int64(replayGrace.Seconds()) && row.SuccessorCache != "" {
				var pair successorPair
				if err := json.Unmarshal([]byte(row.SuccessorCache), &pair); err == nil {
					outAccess = pair.AccessToken
					outRefresh = pair.RefreshToken
					return nil // idempotent success
				}
			}
			// Outside grace OR ≥2 generations old (cache cleared) → revoke family.
			liveIDs, rErr := tx.RefreshSessions().RevokeFamily(ctx, row.FamilyID)
			if rErr != nil {
				return rErr
			}
			_ = appendRevocation(ctx, tx, row.AccountID, row.FamilyID, liveIDs, now)
			return ErrFamilyRevoked
		}

		// Fresh token — rotate (pass tx so all ops use the same connection [R5]).
		a, r, err := i.rotateSession(ctx, tx, row, now)
		if err != nil {
			return err
		}
		outAccess = a
		outRefresh = r
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return outAccess, outRefresh, nil
}

// sha256sum is SHA-256 over b (returns 32-byte slice).
func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// popFromRequest parses the X-PoP-* headers sent by a client on /refresh. Three headers:
//   X-PoP-Timestamp: unix seconds (decimal string)
//   X-PoP-Nonce: base64url-encoded random bytes (≥16 bytes)
//   X-PoP-Sig: base64url-encoded P1363 signature (64 bytes)
func popFromRequest(r *http.Request) (PoPProof, error) {
	tsStr := r.Header.Get("X-PoP-Timestamp")
	nonceB64 := r.Header.Get("X-PoP-Nonce")
	sigB64 := r.Header.Get("X-PoP-Sig")
	if tsStr == "" || nonceB64 == "" || sigB64 == "" {
		return PoPProof{}, ErrPoPMissing
	}
	var ts int64
	if _, err := fmt.Sscanf(tsStr, "%d", &ts); err != nil {
		return PoPProof{}, fmt.Errorf("pop: bad timestamp: %w", err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(nonceB64)
	if err != nil {
		return PoPProof{}, fmt.Errorf("pop: bad nonce: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return PoPProof{}, fmt.Errorf("pop: bad sig: %w", err)
	}
	return PoPProof{Timestamp: ts, Nonce: nonce, Sig: sig}, nil
}
