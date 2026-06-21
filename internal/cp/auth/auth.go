// Package auth provides the CP's identity seam: offline Ed25519 verification of A1's
// AS-signed SessionTokenBody, with a dev-token fallback for local development.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	"spawnery/internal/authsvc/token"
	slogctx "spawnery/internal/log"
)

// Sentinel errors for machine-readable caller dispatch.
var (
	ErrWrongAudience = errors.New("auth: token audience is not \"cp\"")
	ErrRevoked       = errors.New("auth: token or account is revoked")
)

// Identity carries the resolved caller identity from a verified token.
type Identity struct {
	Owner     string // account_id from the token body
	TokenID   string // token_id field (empty for dev-token sessions)
	ExpiresAt time.Time
}

// Verifier authenticates tokens. It tries AS verification first, then falls back to
// dev tokens when devMode is true. Prod (devMode false) = AS tokens only.
type Verifier struct {
	keys    token.KeySet
	dev     map[string]string // token -> owner (dev mode only)
	devMode bool
	now     func() time.Time
	revoked *RevocationRegistry
}

// VerifierConfig holds constructor parameters.
type VerifierConfig struct {
	Keys    token.KeySet
	DevTokens map[string]string // nil/empty = no dev tokens
	DevMode bool
	Now     func() time.Time // nil = time.Now
	Revoked *RevocationRegistry
}

// NewVerifier builds a Verifier from cfg.
func NewVerifier(cfg VerifierConfig) *Verifier {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	dev := cfg.DevTokens
	if dev == nil {
		dev = map[string]string{}
	}
	revoked := cfg.Revoked
	if revoked == nil {
		revoked = NewRevocationRegistry(nil)
	}
	return &Verifier{keys: cfg.Keys, dev: dev, devMode: cfg.DevMode, now: now, revoked: revoked}
}

// Verify authenticates wire token and returns the caller's Identity.
// Order: AS verify (signature + aud=="cp" + revocation check); if the token parse/signature/key/expiry
// check fails AND devMode is on, fall back to the dev-token map (TokenID empty).
// A valid AS token with wrong audience is always refused (ErrWrongAudience), never falls back.
func (v *Verifier) Verify(wire string) (Identity, error) {
	// AS path — attempted whenever there are keys OR we're in prod mode (no keys = only dev tokens).
	if len(v.keys) > 0 {
		body, err := token.Verify(wire, v.keys, v.now())
		if err == nil {
			// Audience check: the caller's job per token.Verify doc [MC2].
			// A valid AS token with wrong aud is always refused — NOT a dev-token candidate.
			if body.Audience != "cp" {
				return Identity{}, ErrWrongAudience
			}
			// Revocation check.
			if v.revoked.IsRevoked(body.TokenId, body.AccountId) {
				return Identity{}, ErrRevoked
			}
			return Identity{
				Owner:     body.AccountId,
				TokenID:   body.TokenId,
				ExpiresAt: time.Unix(body.ExpiresAt, 0),
			}, nil
		}
		// AS verify failed (bad sig/expired/unknown key) — fall through to dev tokens in dev mode.
	}

	// Dev fallback: only when devMode is on.
	if v.devMode {
		owner, ok := v.dev[wire]
		if ok {
			return Identity{Owner: owner, TokenID: "", ExpiresAt: time.Time{}}, nil
		}
	}
	return Identity{}, connect.NewError(connect.CodeUnauthenticated, errUnauth)
}

// --- Context helpers -------------------------------------------------------

type identityKey struct{}
type ownerKey struct{}

// WithIdentity stashes the full Identity on the context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, identityKey{}, id)
	ctx = context.WithValue(ctx, ownerKey{}, id.Owner)
	ctx = slogctx.WithOwnerID(ctx, id.Owner)
	return ctx
}

// IdentityFromContext retrieves the Identity.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok && id.Owner != ""
}

// WithOwner stashes only an owner string (legacy shim; prefer WithIdentity).
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

// OwnerFromContext retrieves the owner string — works whether WithIdentity or WithOwner was used.
func OwnerFromContext(ctx context.Context) (string, bool) {
	o, ok := ctx.Value(ownerKey{}).(string)
	return o, ok && o != ""
}

// bearer extracts the token from an "Authorization: Bearer <t>" header value.
func bearer(h string) string { return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) }

// Interceptor returns a Connect interceptor that verifies requests and stashes the Identity.
func (v *Verifier) Interceptor() connect.Interceptor { return &interceptor{v: v} }

type interceptor struct{ v *Verifier }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		id, err := i.v.Verify(bearer(req.Header().Get("Authorization")))
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithIdentity(ctx, id), req)
	}
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id, err := i.v.Verify(bearer(conn.RequestHeader().Get("Authorization")))
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithIdentity(ctx, id), conn)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no-op
}

var errUnauth = connectError("missing or invalid auth token")

type connectError string

func (e connectError) Error() string { return string(e) }

