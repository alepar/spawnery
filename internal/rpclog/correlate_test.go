package rpclog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	slogctx "spawnery/internal/log"
	"spawnery/internal/rpclog"
)

func TestCorrelationInterceptor_Unary_NoHeader_GeneratesID(t *testing.T) {
	interceptor := rpclog.CorrelationInterceptor()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		slogctx.FromContext(ctx).Info("inside handler")
		return connect.NewResponse(&emptypb.Empty{}), nil
	})

	wrapped := interceptor.WrapUnary(handler)
	req := connect.NewRequest(&emptypb.Empty{})
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	gotID, _ := m["request_id"].(string)
	if gotID == "" {
		t.Errorf("expected non-empty generated request_id in log, got: %v", m)
	}
}

func TestCorrelationInterceptor_Unary_WithHeader_Propagates(t *testing.T) {
	interceptor := rpclog.CorrelationInterceptor()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const wantID = "test-req-id-123"
	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		slogctx.FromContext(ctx).Info("inside handler")
		return connect.NewResponse(&emptypb.Empty{}), nil
	})

	wrapped := interceptor.WrapUnary(handler)
	req := connect.NewRequest(&emptypb.Empty{})
	req.Header().Set("x-request-id", wantID)
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	gotID, _ := m["request_id"].(string)
	if gotID != wantID {
		t.Errorf("request_id: got %q want %q", gotID, wantID)
	}
}

func TestCorrelationInterceptor_Unary_OversizedHeader_GeneratesNew(t *testing.T) {
	interceptor := rpclog.CorrelationInterceptor()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	oversized := strings.Repeat("x", 200)
	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		slogctx.FromContext(ctx).Info("inside handler")
		return connect.NewResponse(&emptypb.Empty{}), nil
	})

	wrapped := interceptor.WrapUnary(handler)
	req := connect.NewRequest(&emptypb.Empty{})
	req.Header().Set("x-request-id", oversized)
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	gotID, _ := m["request_id"].(string)
	if gotID == "" || gotID == oversized {
		t.Errorf("expected generated request_id (not the oversized value), got %q", gotID)
	}
	if len(gotID) > 128 {
		t.Errorf("generated request_id too long: %d chars", len(gotID))
	}
}

func TestCorrelationInterceptor_Unary_SharedIDAcrossLogs(t *testing.T) {
	interceptor := rpclog.CorrelationInterceptor()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	handler := connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		l := slogctx.FromContext(ctx)
		l.Info("first log")
		l.Info("second log")
		return connect.NewResponse(&emptypb.Empty{}), nil
	})

	wrapped := interceptor.WrapUnary(handler)
	req := connect.NewRequest(&emptypb.Empty{})
	req.Header().Set("x-request-id", "shared-id")
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("parse log line %d: %v: %q", i, err, line)
		}
		gotID, _ := m["request_id"].(string)
		if gotID != "shared-id" {
			t.Errorf("line %d: request_id: got %q want shared-id", i, gotID)
		}
	}
}

func TestCorrelationInterceptor_Streaming_WithHeader(t *testing.T) {
	interceptor := rpclog.CorrelationInterceptor()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const wantID = "stream-req-id"
	handler := connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		slogctx.FromContext(ctx).Info("stream handler")
		return nil
	})

	wrapped := interceptor.WrapStreamingHandler(handler)
	conn := &stubConnWithHeader{requestID: wantID}
	if err := wrapped(context.Background(), conn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	gotID, _ := m["request_id"].(string)
	if gotID != wantID {
		t.Errorf("request_id: got %q want %q", gotID, wantID)
	}
}

// stubConnWithHeader extends stubConn with a RequestHeader that injects a request ID.
type stubConnWithHeader struct {
	stubConn
	requestID string
}

func (s *stubConnWithHeader) RequestHeader() http.Header {
	h := make(http.Header)
	if s.requestID != "" {
		h.Set("x-request-id", s.requestID)
	}
	return h
}
