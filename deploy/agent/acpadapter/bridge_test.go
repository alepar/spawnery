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

func TestServeBridgesAndPersistsAcrossReconnect(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "acp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	toAgent, fromAgent := fakeAgent()
	go serve(ln, toAgent, fromAgent)

	// First client.
	dialAndRoundtrip(t, sock, "hello\n")
	// Reconnect: a NEW client must reach the SAME persistent agent.
	dialAndRoundtrip(t, sock, "world\n")
}

func TestServeClientHalfCloseStopsStdinNotStdout(t *testing.T) {
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
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	// Half-close the write side; the echo of "ping\n" must still arrive.
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if got != "ping\n" {
		t.Fatalf("got %q want %q", got, "ping\n")
	}
}
