// Package auth is the demo's stubbed identity: a dev bearer token maps to an
// owner_id. The only piece E4 OAuth later replaces; the rest of the CP is
// owner-id-agnostic.
package auth

import (
	"context"
	"strings"

	"connectrpc.com/connect"
)

type Auth struct{ tokens map[string]string } // token -> owner

func New(tokens map[string]string) *Auth { return &Auth{tokens: tokens} }

func (a *Auth) Owner(token string) (string, bool) {
	o, ok := a.tokens[token]
	return o, ok
}

type ownerKey struct{}

func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}
func OwnerFromContext(ctx context.Context) (string, bool) {
	o, ok := ctx.Value(ownerKey{}).(string)
	return o, ok && o != ""
}

// bearer extracts the token from an "Authorization: Bearer <t>" header value.
func bearer(h string) string { return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) }

// Interceptor authenticates unary + streaming Connect calls: it reads the
// Authorization header, resolves the owner, and stashes it on the context.
func (a *Auth) Interceptor() connect.Interceptor { return &interceptor{a: a} }

type interceptor struct{ a *Auth }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		owner, ok := i.a.Owner(bearer(req.Header().Get("Authorization")))
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithOwner(ctx, owner), req)
	}
}
func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		owner, ok := i.a.Owner(bearer(conn.RequestHeader().Get("Authorization")))
		if !ok {
			return connect.NewError(connect.CodeUnauthenticated, errUnauth)
		}
		return next(WithOwner(ctx, owner), conn)
	}
}
func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no-op
}

var errUnauth = connectError("missing or invalid auth token")

type connectError string

func (e connectError) Error() string { return string(e) }
