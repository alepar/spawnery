package rpclog

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"

	"connectrpc.com/connect"

	slogctx "spawnery/internal/log"
)

// RecoverInterceptor returns a Connect interceptor that recovers from handler panics, converts
// them to connect.CodeInternal responses, and logs the procedure + panic value + stack trace.
// It must be wired OUTERMOST (first argument to connect.WithInterceptors) so it catches panics
// from all inner interceptors and handlers.
//
// http.ErrAbortHandler is re-panicked rather than converted, preserving net/http's intentional
// connection-abort semantics.
func RecoverInterceptor(component string) connect.Interceptor {
	return &recoverInterceptor{component: component}
}

type recoverInterceptor struct{ component string }

func (r *recoverInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) //nolint:forbidigo // intentional re-panic for net/http abort semantics
				}
				slogctx.FromContext(ctx).Error("rpc-panic",
					"component", r.component,
					"procedure", req.Spec().Procedure,
					"panic", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, fmt.Errorf("panic: %v", rec))
			}
		}()
		return next(ctx, req)
	}
}

// WrapStreamingClient is a no-op: recovery is server-side only.
func (r *recoverInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r *recoverInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) //nolint:forbidigo // intentional re-panic for net/http abort semantics
				}
				slogctx.FromContext(ctx).Error("rpc-panic",
					"component", r.component,
					"procedure", conn.Spec().Procedure+" (stream)",
					"panic", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, fmt.Errorf("panic: %v", rec))
			}
		}()
		return next(ctx, conn)
	}
}
