package rpclog

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	slogctx "spawnery/internal/log"
)

// maxRequestIDLen is the maximum accepted length for an inbound x-request-id header value.
// Values longer than this are treated as absent (a new ID is generated). Slog escapes header
// values before logging, so injection is not a concern; the cap prevents log bloat only.
const maxRequestIDLen = 128

// CorrelationInterceptor returns a Connect interceptor that reads the x-request-id request header
// and stores it on the context via log.WithRequestID so every log emitted within the handler
// carries a request_id attribute.  If the header is absent or longer than maxRequestIDLen, a new
// UUIDv4 is generated.  Must be wired OUTSIDE RecoverInterceptor so rpc-panic logs also carry the
// request_id.
func CorrelationInterceptor() connect.Interceptor {
	return &correlationInterceptor{}
}

type correlationInterceptor struct{}

func (c *correlationInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx = withRequestID(ctx, req.Header().Get("x-request-id"))
		return next(ctx, req)
	}
}

// WrapStreamingClient is a no-op: correlation is server-side only.
func (c *correlationInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (c *correlationInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx = withRequestID(ctx, conn.RequestHeader().Get("x-request-id"))
		return next(ctx, conn)
	}
}

func withRequestID(ctx context.Context, incoming string) context.Context {
	id := strings.TrimSpace(incoming)
	if id == "" || len(id) > maxRequestIDLen {
		id = uuid.NewString()
	}
	return slogctx.WithRequestID(ctx, id)
}
