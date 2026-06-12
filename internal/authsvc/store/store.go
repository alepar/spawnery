// Package store is the AS's durable identity layer: users, refresh-session families, OAuth
// flow state, device grants, and the revocation feed (auth-identity design §1/§3). It mirrors
// internal/cp/store's driver pattern (bun + goose migrations) but is sqlite-only: the AS is
// sqlite by spec [AM13] — tier-0 data, replicated (see deploy/authsvc/README.md).
package store

import (
	"context"
	"errors"
)

var (
	ErrConflict = errors.New("authsvc/store: conflict")
	ErrNotFound = errors.New("authsvc/store: not found")
)

// Config selects the backend. Driver is "sqlite" (only).
type Config struct {
	Driver string
	DSN    string
}

type UserRepo interface {
	GetBySub(ctx context.Context, githubSub int64) (User, error)
	GetByID(ctx context.Context, accountID string) (User, error)
	Create(ctx context.Context, u User) error
	SetStatus(ctx context.Context, accountID, status string) error
	SetHandle(ctx context.Context, accountID, handle string) error
}

type RefreshSessionRepo interface {
	Get(ctx context.Context, tokenHash string) (RefreshSession, error)
	Insert(ctx context.Context, s RefreshSession) error
	// Supersede stamps the predecessor (superseded_by/at + successor_cache), clears the
	// successor_cache of every OTHER row in the family (so only the most recently superseded
	// row can replay), and inserts the successor row. Run inside WithTx.
	Supersede(ctx context.Context, predecessorHash string, successor RefreshSession, successorCache string, now int64) error
	// RevokeFamily marks every row of the family revoked and returns the access_token_ids
	// that were live (non-revoked) — the revocation-event payload.
	RevokeFamily(ctx context.Context, familyID string) ([]string, error)
	// CountFamilies counts distinct non-revoked families for the account.
	CountFamilies(ctx context.Context, accountID string) (int, error)
	// OldestFamily returns the non-revoked family with the earliest family_created_at.
	OldestFamily(ctx context.Context, accountID string) (string, error)
	DeleteExpired(ctx context.Context, now int64) (int, error)
}

type OAuthStateRepo interface {
	Create(ctx context.Context, s OAuthState) error
	// Consume atomically marks the state used and returns it; a second consume (or a missing/
	// already-used state) is ErrNotFound — single-use by construction [AM8].
	Consume(ctx context.Context, state string) (OAuthState, error)
}

type DeviceGrantRepo interface {
	Create(ctx context.Context, g DeviceGrant) error
	GetByUserCode(ctx context.Context, userCode string) (DeviceGrant, error)
	Get(ctx context.Context, deviceCodeHash string) (DeviceGrant, error)
	// SetDecision moves pending -> approved|denied, binding the deciding account. ErrConflict
	// if the grant is not pending.
	SetDecision(ctx context.Context, userCode, accountID, status string) error
	// Redeem atomically moves approved -> redeemed; ErrConflict if not approved (double redeem).
	Redeem(ctx context.Context, deviceCodeHash string) (DeviceGrant, error)
	// BumpAttempt increments attempt_count and returns the new value (confirm/peek probing
	// lockout).
	BumpAttempt(ctx context.Context, deviceCodeHash string) (int, error)
	SetLastPolled(ctx context.Context, deviceCodeHash string, now int64) error
	SetStatus(ctx context.Context, deviceCodeHash, status string) error
}

type RevocationRepo interface {
	// Append inserts an event and returns its assigned monotonically increasing seq.
	Append(ctx context.Context, ev RevocationEvent) (int64, error)
	Since(ctx context.Context, seq int64) ([]RevocationEvent, error)
}

type Store interface {
	Users() UserRepo
	RefreshSessions() RefreshSessionRepo
	OAuthStates() OAuthStateRepo
	DeviceGrants() DeviceGrantRepo
	Revocations() RevocationRepo
	// WithTx runs fn in a transaction. If called inside an existing WithTx, fn runs in the
	// SAME transaction (flat composition — no savepoints; an inner error rolls back the whole tx).
	WithTx(ctx context.Context, fn func(tx Store) error) error
	Close() error
}
