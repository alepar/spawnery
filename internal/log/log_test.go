package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestNewHandlerJSON verifies that format=="json" produces a valid JSON line
// with time, level, msg keys and any supplied attribute.
func TestNewHandlerJSON(t *testing.T) {
	var buf bytes.Buffer
	h := newHandler(&buf, "json")
	l := slog.New(h)
	l.Info("hello", "key", "value")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected non-empty output")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v: %q", err, line)
	}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in JSON output: %v", k, m)
		}
	}
	if m["key"] != "value" {
		t.Errorf("attr key=value not in output: %v", m)
	}
}

// TestNewHandlerText verifies that format=="" and format=="text" both produce
// a text line containing level=INFO and the message string.
func TestNewHandlerText(t *testing.T) {
	for _, format := range []string{"text", ""} {
		format := format
		t.Run("format="+format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			h := newHandler(&buf, format)
			l := slog.New(h)
			l.Info("hello world")

			out := buf.String()
			if !strings.Contains(out, "level=INFO") {
				t.Errorf("expected level=INFO in text output: %q", out)
			}
			if !strings.Contains(out, "hello world") {
				t.Errorf("expected message in text output: %q", out)
			}
		})
	}
}
