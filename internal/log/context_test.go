package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestFromContext_AllFields(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-1")
	ctx = WithSpawnID(ctx, "sp-1")
	ctx = WithSessionID(ctx, "sess-1")
	ctx = WithOwnerID(ctx, "owner-1")

	FromContext(ctx).Info("test")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	want := map[string]string{
		"request_id": "req-1",
		"spawn_id":   "sp-1",
		"session_id": "sess-1",
		"owner":      "owner-1",
	}
	for k, wv := range want {
		if got, _ := m[k].(string); got != wv {
			t.Errorf("attr %q: got %q want %q (full: %v)", k, got, wv, m)
		}
	}
}

func TestFromContext_Empty(t *testing.T) {
	// FromContext on a bare background context must not panic and must return a usable logger.
	l := FromContext(context.Background())
	if l == nil {
		t.Fatal("expected non-nil logger from empty context")
	}
	// Logging with it must not panic.
	l.Info("ok")
}

func TestFromContext_EmptyStringsOmitted(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	// Empty-string setters are no-ops; they must not inject blank attrs.
	ctx := WithRequestID(context.Background(), "")
	ctx = WithSpawnID(ctx, "")
	FromContext(ctx).Info("test")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("parse log: %v: %q", err, buf.String())
	}
	for _, k := range []string{"request_id", "spawn_id", "session_id", "owner"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected attr %q in log for context with no IDs: %v", k, m)
		}
	}
}
