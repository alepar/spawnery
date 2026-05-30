package acp

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	out := Message{JSONRPC: "2.0", ID: intptr(1), Method: "initialize"}
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
	if got.Method != "initialize" || got.ID == nil || *got.ID != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func intptr(i int) *int { return &i }
