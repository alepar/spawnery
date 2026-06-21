// Package log provides structured logging initialisation for spawnery processes.
// It wraps log/slog and exposes a single Init function that sets the process-wide
// default logger from environment variables.
package log

import (
	"io"
	"log/slog"
	"os"
)

// newHandler returns a slog.Handler writing to w.
// format=="json" -> JSONHandler; anything else (including "") -> TextHandler.
// Both handlers emit at LevelInfo and above (time + level + msg by default).
func newHandler(w io.Writer, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// Init sets the process-wide slog default from the environment.
// get is typically os.Getenv; it reads CP_LOG_FORMAT ("json" → JSON, anything else → text).
// Call once, as the very first statement of main().
func Init(get func(string) string) {
	slog.SetDefault(slog.New(newHandler(os.Stderr, get("CP_LOG_FORMAT"))))
}
