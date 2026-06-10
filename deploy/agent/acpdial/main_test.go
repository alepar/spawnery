package main

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDialAddr checks the addr resolution precedence: arg > env > default.
func TestDialAddr(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("ACP_DIAL", "")
		if got := dialAddr(nil); got != "127.0.0.1:7000" {
			t.Fatalf("default = %q", got)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("ACP_DIAL", "10.0.0.5:9000")
		if got := dialAddr(nil); got != "10.0.0.5:9000" {
			t.Fatalf("env = %q", got)
		}
	})
	t.Run("arg-wins", func(t *testing.T) {
		t.Setenv("ACP_DIAL", "10.0.0.5:9000")
		if got := dialAddr([]string{"1.2.3.4:5"}); got != "1.2.3.4:5" {
			t.Fatalf("arg = %q", got)
		}
	})
}

// TestRunRoundTrip proves the shim relays both directions against a fake ACP
// server (an echo server): bytes written to `in` reach the server, and the
// server's echo comes back out of `out`. Then closing `in` (nori exit / stdin
// EOF) makes run() return cleanly — proving the deadlock-safe Close unblocks the
// other copy (no hang).
func TestRunRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Fake server: echo everything back (stands in for acpmux echoing frames).
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// `in` carries one message then EOFs (like nori sending then closing stdin).
	const msg = `{"jsonrpc":"2.0","method":"initialize"}` + "\n"
	in := strings.NewReader(msg)
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- run(conn, in, &out) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: run did not return after stdin EOF (deadlock-safe Close missing?)")
	}

	if got := out.String(); got != msg {
		t.Fatalf("round-trip mismatch: sent %q got echoed %q", msg, got)
	}
}

// TestRunServerCloseUnblocks proves the OTHER direction of the deadlock fix: if
// the server (acpmux) closes the conn first while `in` is still open/blocked
// (nori idle, not sending), run() still returns — the conn->out copy's Close
// unblocks the in->conn copy. We use a blocking reader for `in` that would hang
// forever if not interrupted.
func TestRunServerCloseUnblocks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Server accepts then immediately closes (acpmux died / goose crashed).
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_ = conn.Close()
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- run(conn, blockingReader{}, io.Discard) }()

	select {
	case <-done:
		// returned (nil or error) — the point is it did NOT hang.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: run did not return after server closed conn (in->conn copy not unblocked)")
	}
}

// blockingReader blocks forever on Read, modeling nori's stdin sitting idle.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }
