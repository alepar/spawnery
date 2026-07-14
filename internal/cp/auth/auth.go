// Package auth provides the CP's root-anchored identity verification seam.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"

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
	IssuedAt  int64
	ExpiresAt time.Time
}

// Verifier authenticates certified session artifacts and exact opaque development tokens.
type Verifier struct {
	artifacts *token.Verifier
	dev       map[string]string // token -> owner (dev mode only)
	devMode   bool
	now       func() time.Time
	revoked   *RevocationRegistry
}

// VerifierConfig holds constructor parameters.
type VerifierConfig struct {
	Artifacts *token.Verifier
	DevTokens map[string]string // nil/empty = no dev tokens
	DevMode   bool
	Now       func() time.Time // nil = time.Now
	Revoked   *RevocationRegistry
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
	return &Verifier{artifacts: cfg.Artifacts, dev: dev, devMode: cfg.DevMode, now: now, revoked: revoked}
}

// Verify authenticates wire token and returns the caller's Identity.
// The artifact envelope is authenticated before its payload is parsed or acted on.
func (v *Verifier) Verify(wire string) (Identity, error) {
	if v.artifacts != nil {
		now := v.now()
		payload, err := v.artifacts.Verify(wire, token.ArtifactTypeSession, now)
		if err == nil {
			var body authv1.SessionTokenBody
			if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, &body); err != nil || len(body.ProtoReflect().GetUnknown()) != 0 {
				return Identity{}, connect.NewError(connect.CodeUnauthenticated, errUnauth)
			}
			if !now.Before(time.Unix(body.ExpiresAt, 0)) || time.Unix(body.IssuedAt, 0).After(now.Add(time.Minute)) {
				return Identity{}, connect.NewError(connect.CodeUnauthenticated, errUnauth)
			}
			// Audience check: the caller's job per token.Verify doc [MC2].
			// A valid AS token with wrong aud is always refused — NOT a dev-token candidate.
			if body.Audience != "cp" {
				return Identity{}, ErrWrongAudience
			}
			// Revocation check.
			if v.revoked.IsRevoked(body.TokenId, body.AccountId, body.IssuedAt, now) {
				return Identity{}, ErrRevoked
			}
			return Identity{
				Owner: body.AccountId, TokenID: body.TokenId, IssuedAt: body.IssuedAt, ExpiresAt: time.Unix(body.ExpiresAt, 0),
			}, nil
		}
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
