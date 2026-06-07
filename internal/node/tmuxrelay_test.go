package node

import "testing"

func TestParseClientFrame(t *testing.T) {
	// input opcode
	kind, data, cols, rows := parseClientFrame([]byte{tmuxOpInput, 'h', 'i'})
	if kind != tmuxOpInput || string(data) != "hi" {
		t.Fatalf("input: kind=%d data=%q", kind, data)
	}
	// resize opcode
	kind, _, cols, rows = parseClientFrame(append([]byte{tmuxOpResize}, []byte("120 40")...))
	if kind != tmuxOpResize || cols != 120 || rows != 40 {
		t.Fatalf("resize: kind=%d cols=%d rows=%d", kind, cols, rows)
	}
	// empty frame → treated as input (no-op), never panics
	if k, _, _, _ := parseClientFrame(nil); k != tmuxOpInput {
		t.Fatalf("empty frame should default to input")
	}
}
