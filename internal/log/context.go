package log

import (
	"context"
	"log/slog"
)

type contextKey int

const (
	keyRequestID contextKey = iota
	keySpawnID
	keySessionID
	keyOwnerID
)

// WithRequestID returns a new context with the request ID attached (no-op if id is empty).
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keyRequestID, id)
}

// WithSpawnID returns a new context with the spawn ID attached (no-op if id is empty).
func WithSpawnID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keySpawnID, id)
}

// WithSessionID returns a new context with the session ID attached (no-op if id is empty).
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keySessionID, id)
}

// WithOwnerID returns a new context with the owner ID attached (no-op if id is empty).
func WithOwnerID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keyOwnerID, id)
}

// FromContext returns slog.Default() enriched with any correlation IDs present in ctx
// (request_id, spawn_id, session_id, owner). Fields absent from ctx are omitted.
func FromContext(ctx context.Context) *slog.Logger {
	var attrs []any
	if v, ok := ctx.Value(keyRequestID).(string); ok && v != "" {
		attrs = append(attrs, "request_id", v)
	}
	if v, ok := ctx.Value(keySpawnID).(string); ok && v != "" {
		attrs = append(attrs, "spawn_id", v)
	}
	if v, ok := ctx.Value(keySessionID).(string); ok && v != "" {
		attrs = append(attrs, "session_id", v)
	}
	if v, ok := ctx.Value(keyOwnerID).(string); ok && v != "" {
		attrs = append(attrs, "owner", v)
	}
	if len(attrs) == 0 {
		return slog.Default()
	}
	return slog.Default().With(attrs...)
}
