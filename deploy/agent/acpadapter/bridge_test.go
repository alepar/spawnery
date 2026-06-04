package main

import (
	"bufio"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeAgent returns (toAgent, fromAgent) wired as an echo: bytes written to
// toAgent come back on fromAgent. Models a persistent agent process stdio.
func fakeAgent() (io.Writer, io.Reader) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { _, _ = io.Copy(outW, inR) }() // echo stdin -> stdout, lives for the whole test
	return inW, outR
}

func dialAndRoundtrip(t *testing.T, path, line string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != line {
		t.Fatalf("got %q want %q", got, line)
	}
}

// The adapter bridges raw bytes both ways and keeps the agent alive across a reconnect, so a NEW
// client reaches the SAME persistent agent.
func TestServeBridgesAndPersistsAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	dialAndRoundtrip(t, sock, "hello\n")
	dialAndRoundtrip(t, sock, "world\n")
}

// The thin adapter must NOT parse ACP or inject broker frames (spawn/turn, spawn/history). A
// session/prompt sent through is echoed back verbatim with nothing prepended or appended.
func TestServeDoesNotInjectBrokerFrames(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	prompt := `{"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}` + "\n"
	if _, err := io.WriteString(c, prompt); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != prompt {
		t.Fatalf("first line got %q want the echoed prompt verbatim (no broker frame)", got)
	}
	// No second line should arrive — a broker would have injected a spawn/turn frame.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if extra, err := r.ReadString('\n'); err == nil {
		t.Fatalf("unexpected extra frame after the echo: %q", extra)
	}
}

// Goose output produced while no node is attached is buffered and flushed, IN ORDER, to the next
// connection — and live output after attach follows strictly behind the flushed buffer.
func TestConnHubBuffersWhileDetachedAndFlushesOnAttach(t *testing.T) {
	h := &connHub{}
	h.write([]byte("a\n")) // no conn attached -> buffered
	h.write([]byte("b\n"))

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan struct{})
	go func() {
		h.attach(server) // flushes "a\n","b\n" to server (blocks until read; net.Pipe is synchronous)
		h.write([]byte("c\n"))
		close(done)
	}()

	r := bufio.NewReader(client)
	for _, want := range []string{"a\n", "b\n", "c\n"} {
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != want {
			t.Fatalf("got %q want %q (order must be buffered-then-live)", got, want)
		}
	}
	<-done
}

// Past the byte cap the gap buffer evicts OLDEST WHOLE LINES, never delivering a torn line.
func TestConnHubEvictsOldestWholeLinesPastCap(t *testing.T) {
	h := &connHub{}
	mkLine := func(c byte) []byte {
		b := make([]byte, 100*1024) // 102400 bytes; 12 of these = 1,228,800 > maxBufBytes (1,048,576)
		for i := range b {
			b[i] = c
		}
		b[len(b)-1] = '\n'
		return b
	}
	for i := 0; i < 12; i++ {
		h.write(mkLine(byte('A' + i)))
	}

	h.mu.Lock()
	n, count := h.n, len(h.buf)
	first := h.buf[0][0]
	bufCopy := append([][]byte(nil), h.buf...)
	h.mu.Unlock()

	if n > maxBufBytes {
		t.Fatalf("buffer holds %d bytes, exceeds cap %d", n, maxBufBytes)
	}
	// 1,228,800 -> evict A -> 1,126,400 (still > cap) -> evict B -> 1,024,000 (<= cap). 10 lines, first 'C'.
	if count != 10 {
		t.Fatalf("want 10 buffered lines after eviction, got %d", count)
	}
	if first != 'C' {
		t.Fatalf("oldest KEPT line should start with 'C' (A,B evicted), got %q", first)
	}
	for i, b := range bufCopy {
		if len(b) == 0 || b[len(b)-1] != '\n' {
			t.Fatalf("buffered line %d is torn (no trailing newline)", i)
		}
	}
}
