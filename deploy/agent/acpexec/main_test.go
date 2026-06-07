package main

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// TestServeEchoesAndLoops proves the bridge:
//   - wires the accepted connection to the agent's stdin/stdout (here `cat`,
//     which echoes stdin->stdout), and
//   - loops to accept a second connection with a fresh child after the first
//     disconnects.
func TestServeEchoesAndLoops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	go func() { _ = serve(ln, []string{"cat"}) }()

	// First connection: write a line, expect it echoed back by `cat`.
	echoOnce(t, addr, "hello\n")

	// Second connection: the loop must have accepted a fresh child after the
	// first connection closed, and it must echo too.
	echoOnce(t, addr, "world\n")
}

// echoOnce dials addr, writes line, asserts it is echoed back, then closes.
func echoOnce(t *testing.T, addr, line string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != line {
		t.Fatalf("echo mismatch: want %q, got %q", line, got)
	}
}

func TestListenSpec(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		t.Setenv("ACP_LISTEN", "tcp://0.0.0.0:7000")
		net, addr := listenSpec()
		if net != "tcp" || addr != "0.0.0.0:7000" {
			t.Fatalf("tcp: got (%q,%q)", net, addr)
		}
	})
	t.Run("unix", func(t *testing.T) {
		t.Setenv("ACP_LISTEN", "unix:/tmp/acp.sock")
		net, addr := listenSpec()
		if net != "unix" || addr != "/tmp/acp.sock" {
			t.Fatalf("unix: got (%q,%q)", net, addr)
		}
	})
	t.Run("default-abstract", func(t *testing.T) {
		t.Setenv("ACP_LISTEN", "")
		t.Setenv("ACP_SOCKET", "")
		net, addr := listenSpec()
		if net != "unix" || addr != "@spawnlet-acp" {
			t.Fatalf("default: got (%q,%q)", net, addr)
		}
	})
	t.Run("acp-socket-override", func(t *testing.T) {
		t.Setenv("ACP_LISTEN", "")
		t.Setenv("ACP_SOCKET", "/run/custom.sock")
		net, addr := listenSpec()
		if net != "unix" || addr != "/run/custom.sock" {
			t.Fatalf("override: got (%q,%q)", net, addr)
		}
	})
}
