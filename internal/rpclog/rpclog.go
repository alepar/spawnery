// Package rpclog provides a Connect interceptor that logs every handler error so client-facing
// API failures (web UI, spawnctl) are never silently swallowed — including the full wrapped
// error chain (which identifies the root cause) and a stack trace.
package rpclog

import (
	"context"
	"runtime/debug"

	"connectrpc.com/connect"

	slogctx "spawnery/internal/log"
)

// Interceptor returns a Connect interceptor that logs errors returned by unary and streaming
// handlers. component is a short label for the emitting process ("cp" / "node"). It logs the RPC
// procedure, the Connect error code, the full wrapped error chain (err.Error() includes every
// %w-wrapped layer's context, e.g. "suspend: finish: capture delta: ... container is paused"),
// and a goroutine stack at the server boundary.
//
// NOTE on "root cause stack": Go's stdlib errors carry a wrapped *message* chain but not an
// origin stack trace. This logs the chain (which names the failing layer) plus the boundary
// stack; a true origin stack would require stacktrace-carrying errors at each return site.
func Interceptor(component string) connect.Interceptor {
	return &interceptor{component: component}
}

type interceptor struct{ component string }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		if err != nil {
			i.logErr(ctx, req.Spec().Procedure, "", err)
		}
		return resp, err
	}
}

// WrapStreamingClient is a no-op: this interceptor logs server-side handler errors only.
func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		if err != nil {
			i.logErr(ctx, conn.Spec().Procedure, " (stream)", err)
		}
		return err
	}
}

func (i *interceptor) logErr(ctx context.Context, procedure, kind string, err error) {
	slogctx.FromContext(ctx).Error("rpc-error",
		"component", i.component,
		"procedure", procedure+kind,
		"code", connect.CodeOf(err).String(),
		"err", err,
		"stack", string(debug.Stack()),
	)
}
