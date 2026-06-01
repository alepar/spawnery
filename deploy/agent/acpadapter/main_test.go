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
