package main

import (
	"bufio"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAdapterBinaryBridgesToStubAgent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "acpadapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(dir, "acp.sock")

	cmd := exec.Command(bin, "cat")
	cmd.Env = append(cmd.Environ(), "ACP_SOCKET="+sock)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for the socket to appear.
	var c net.Conn
	for i := 0; i < 100; i++ {
		var err error
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("adapter never bound the socket")
	}
	defer c.Close()

	if _, err := io.WriteString(c, "echo-me\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "echo-me\n" {
		t.Fatalf("got %q want %q", got, "echo-me\n")
	}
}

// TestAdapterBinaryBridgesOverTCP covers the CRI/runsc transport: ACP_LISTEN=tcp://... makes the
// adapter listen on TCP (the node dials the pod IP because gVisor isolates the abstract UDS).
func TestAdapterBinaryBridgesOverTCP(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "acpadapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Pick a free port, then hand it to the adapter.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cmd := exec.Command(bin, "cat")
	cmd.Env = append(cmd.Environ(), "ACP_LISTEN=tcp://"+addr)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer cmd.Process.Kill()

	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("adapter never bound the tcp port")
	}
	defer c.Close()

	if _, err := io.WriteString(c, "echo-me\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "echo-me\n" {
		t.Fatalf("got %q want %q", got, "echo-me\n")
	}
}
