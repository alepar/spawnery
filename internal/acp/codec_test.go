package acp

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	out := Message{JSONRPC: "2.0", ID: IntID(1), Method: "initialize"}
	if err := WriteMessage(&buf, out); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("expected newline framing, got %q", buf.String())
	}
	r := NewReader(&buf)
	got, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n, ok := got.ID.AsInt(); got.Method != "initialize" || !ok || n != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestRawIDStringRoundTrip proves a JSON-RPC string id (as nori, the Rust ACP
// TUI, uses) round-trips VERBATIM and that AsInt reports it is not an integer.
func TestRawIDStringRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []byte(`{"jsonrpc":"2.0","id":"abc-123","method":"initialize"}` + "\n")
	buf.Write(in)
	got, err := NewReader(&buf).ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ID == nil || string(*got.ID) != `"abc-123"` {
		t.Fatalf("string id not preserved: %+v", got.ID)
	}
	if _, ok := got.ID.AsInt(); ok {
		t.Fatalf("string id must not parse as int")
	}
	// Re-marshal must echo the same string id back.
	var out bytes.Buffer
	if err := WriteMessage(&out, got); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"id":"abc-123"`)) {
		t.Fatalf("string id not echoed verbatim: %s", out.String())
	}
}
