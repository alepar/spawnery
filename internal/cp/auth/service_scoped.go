package auth

import (
	"context"
	"crypto/subtle"

	"connectrpc.com/connect"
)

const ServiceSecretHeader = "X-Spawnery-AS-Secret"

type serviceScopedInterceptor struct {
	v        *Verifier
	header   string
	secret   string
	allowed  map[string]struct{}
	fallback connect.Interceptor
}

func NewServiceScopedInterceptor(v *Verifier, header, secret string, allowedProcedures ...string) connect.Interceptor {
	allowed := make(map[string]struct{}, len(allowedProcedures))
	for _, procedure := range allowedProcedures {
		allowed[procedure] = struct{}{}
	}
	return &serviceScopedInterceptor{
		v:        v,
		header:   header,
		secret:   secret,
		allowed:  allowed,
		fallback: v.Interceptor(),
	}
}

func (i *serviceScopedInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	userNext := i.fallback.WrapUnary(next)
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if i.serviceAuthorized(req) {
			return next(ctx, req)
		}
		return userNext(ctx, req)
	}
}

func (i *serviceScopedInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return i.fallback.WrapStreamingHandler(next)
}

func (i *serviceScopedInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *serviceScopedInterceptor) serviceAuthorized(req connect.AnyRequest) bool {
	if i.secret == "" || i.header == "" {
		return false
	}
	if _, ok := i.allowed[req.Spec().Procedure]; !ok {
		return false
	}
	got := req.Header().Get(i.header)
	return subtle.ConstantTimeCompare([]byte(got), []byte(i.secret)) == 1
}
