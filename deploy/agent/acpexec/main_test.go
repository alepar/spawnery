package main

import (
	"bufio"
	"io"
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

// TestServeChildExitFirstAcceptsReconnect is the regression test for the .16
// review deadlock: a child that exits ON ITS OWN (here `sh -c 'printf done; exit
// 0'`) while the node keeps its conn OPEN and idle. With the buggy direct
// cmd.Stdin/Stdout = conn wiring, os/exec's internal node->child copy stays
// blocked on conn.Read(), cmd.Wait() never returns, runChild never returns, and
// the accept loop is wedged — so the second dial below never gets serviced and
// this test times out. With the pipe-based fix, cmd.Wait() returns the instant
// the child exits, conn is closed, and the loop accepts the reconnect.
func TestServeChildExitFirstAcceptsReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// First child exits on its own; the SECOND child echoes so we can prove the
	// reconnect is actually serviced. Each accept runs a fresh child of this argv,
	// so make the FIRST behave as exit-on-own and the rest as cat by serving two
	// different argvs via two sequential serve calls would be awkward; instead use
	// a single argv that exits on its own for connection #1 and rely on echo for
	// #2 not being possible with one argv. So: use exit-on-own argv for the whole
	// listener and assert connection #2 merely COMPLETES (child runs + EOF) within
	// the timeout, which is enough to prove the loop wasn't wedged.
	go func() { _ = serve(ln, []string{"sh", "-c", "printf done; exit 0"}) }()

	// Connection #1: dial, read the child's "done" output, but DO NOT close the
	// conn — keep it open and idle, exactly like the node's long-lived Pump. The
	// child exits before we ever close our end.
	conn1, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial #1: %v", err)
	}
	// Keep conn1 open for the lifetime of the test; closing it would mask the bug.
	defer conn1.Close()
	_ = conn1.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn1, buf); err != nil {
		t.Fatalf("read #1: %v", err)
	}
	if string(buf) != "done" {
		t.Fatalf("read #1: want %q, got %q", "done", string(buf))
	}

	// Connection #2: a SECOND dial to the same listener must be accepted and
	// serviced even though conn1 is still open. Against the buggy code the accept
	// loop is wedged on runChild and this hangs; we bound it with a timeout.
	done := make(chan error, 1)
	go func() {
		conn2, derr := net.DialTimeout("tcp", addr, 5*time.Second)
		if derr != nil {
			done <- derr
			return
		}
		defer conn2.Close()
		// The fresh child writes "done" then exits, then runChild closes conn2 ->
		// we observe child output followed by EOF. Reading to EOF proves the child
		// ran and the connection was fully serviced.
		_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
		out, rerr := io.ReadAll(conn2)
		if rerr != nil {
			done <- rerr
			return
		}
		if string(out) != "done" {
			done <- &testErr{msg: "conn2 output mismatch: got " + string(out)}
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second connection not serviced: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: accept loop wedged after child exited with conn still open (deadlock regression)")
	}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

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
