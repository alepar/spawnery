package rpclog_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"spawnery/internal/rpclog"
)

// stubConn is a minimal connect.StreamingHandlerConn for testing.
type stubConn struct{ connect.StreamingHandlerConn }

func (s *stubConn) Spec() connect.Spec { return connect.Spec{} }

func TestRecoverInterceptor_Unary_Panic(t *testing.T) {
	interceptor := rpclog.RecoverInterceptor("test")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	panicHandler := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		panic("boom")
	})
	wrapped := interceptor.WrapUnary(panicHandler)

	req := connect.NewRequest(&emptypb.Empty{})
	resp, err := wrapped(context.Background(), req)

	if resp != nil {
		t.Errorf("expected nil response on panic, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected non-nil error on panic, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("expected CodeInternal, got %v", code)
	}

	logged := buf.String()
	count := strings.Count(logged, "rpc-panic")
	if count != 1 {
		t.Errorf("expected rpc-panic logged exactly once, got %d occurrences in: %q", count, logged)
	}
}

func TestRecoverInterceptor_Streaming_Panic(t *testing.T) {
	interceptor := rpclog.RecoverInterceptor("test")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	panicHandler := connect.StreamingHandlerFunc(func(_ context.Context, _ connect.StreamingHandlerConn) error {
		panic("stream-boom")
	})
	wrapped := interceptor.WrapStreamingHandler(panicHandler)

	err := wrapped(context.Background(), &stubConn{})

	if err == nil {
		t.Fatal("expected non-nil error on panic, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("expected CodeInternal, got %v", code)
	}

	logged := buf.String()
	count := strings.Count(logged, "rpc-panic")
	if count != 1 {
		t.Errorf("expected rpc-panic logged exactly once, got %d occurrences in: %q", count, logged)
	}
}

func TestRecoverInterceptor_Unary_AbortHandler(t *testing.T) {
	interceptor := rpclog.RecoverInterceptor("test")

	panicHandler := connect.UnaryFunc(func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		panic(http.ErrAbortHandler)
	})
	wrapped := interceptor.WrapUnary(panicHandler)

	req := connect.NewRequest(&emptypb.Empty{})

	defer func() {
		r := recover()
		if r != http.ErrAbortHandler {
			t.Errorf("expected http.ErrAbortHandler to re-panic, got %v", r)
		}
	}()
	_, _ = wrapped(context.Background(), req)
}

func TestRecoverInterceptor_Unary_NoError(t *testing.T) {
	interceptor := rpclog.RecoverInterceptor("test")

	okHandler := connect.UnaryFunc(func(_ context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	wrapped := interceptor.WrapUnary(okHandler)

	req := connect.NewRequest(&emptypb.Empty{})
	resp, err := wrapped(context.Background(), req)

	if err != nil {
		t.Errorf("expected nil error for non-panicking handler, got %v", err)
	}
	if resp == nil {
		t.Error("expected non-nil response for non-panicking handler")
	}
}
