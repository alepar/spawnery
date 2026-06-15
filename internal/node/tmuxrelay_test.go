package node

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTmuxRelayActivityTracking verifies that attached()/lastActive()/markActive() behave correctly:
// a fresh relay starts with recent activity and no clients; markActive refreshes the clock; attached()
// reflects clients. The node now reports these as metrics to the CP (§6); the CP drives idle decisions.
func TestTmuxRelayActivityTracking(t *testing.T) {
	relay := newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })

	// A new relay starts with recent activity (not the zero time).
	if time.Since(relay.lastActive()) > time.Second {
		t.Fatal("new relay should start with recent activity")
	}

	// attached() returns false with no clients.
	if relay.attached() {
		t.Fatal("no clients -> not attached")
	}

	// markActive refreshes the clock.
	old := time.Now().Add(-time.Hour)
	relay.mu.Lock()
	relay.lastActivity = old
	relay.mu.Unlock()
	relay.markActive()
	if !relay.lastActive().After(old) {
		t.Fatal("markActive must refresh lastActivity")
	}
}

func TestParseClientFrame(t *testing.T) {
	// input opcode
	kind, data, _, _ := parseClientFrame([]byte{tmuxOpInput, 'h', 'i'})
	if kind != tmuxOpInput || string(data) != "hi" {
		t.Fatalf("input: kind=%d data=%q", kind, data)
	}
	// resize opcode
	kind, _, cols, rows := parseClientFrame(append([]byte{tmuxOpResize}, []byte("120 40")...))
	if kind != tmuxOpResize || cols != 120 || rows != 40 {
		t.Fatalf("resize: kind=%d cols=%d rows=%d", kind, cols, rows)
	}
	// empty frame → treated as input (no-op), never panics
	if k, _, _, _ := parseClientFrame(nil); k != tmuxOpInput {
		t.Fatalf("empty frame should default to input")
	}
}

func TestTmuxRelayForkBarrierBlocksInputUntilRelease(t *testing.T) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFile.Close()
	defer writeFile.Close()

	relay := newTmuxRelay([]string{"true"}, func(string, []byte) error { return nil })
	relay.mu.Lock()
	relay.clients["c1"] = &tmuxClient{ptmx: writeFile}
	relay.mu.Unlock()

	barrier := forkIngressBarrier{sourceGeneration: 9, transferSetID: "ts-1"}
	if !relay.tryAcquireForkBarrier(barrier) {
		t.Fatal("first tmux fork barrier acquire failed")
	}
	relay.fromClient("c1", append([]byte{tmuxOpInput}, []byte("blocked")...))
	assertNoPipeData(t, readFile)

	relay.releaseForkBarrier(func(b forkIngressBarrier) bool { return b.matches(barrier) })
	assertPipeData(t, readFile, "blocked")
	relay.fromClient("c1", append([]byte{tmuxOpInput}, []byte("after")...))
	assertPipeData(t, readFile, "after")
}

func assertNoPipeData(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	_ = f.SetReadDeadline(time.Time{})
	if n > 0 {
		t.Fatalf("unexpected tmux input while fork barrier held: %q", buf[:n])
	}
	if err == nil {
		t.Fatal("read unexpectedly succeeded with no data")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) && !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("read error = %v, want timeout", err)
	}
}

func assertPipeData(t *testing.T, f *os.File, want string) {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := f.Read(buf)
	_ = f.SetReadDeadline(time.Time{})
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("tmux input after release = %q, want %q", got, want)
	}
}
