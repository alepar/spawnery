package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAdapterBinarySpeaksACP builds the adapter, points it at a trivial fake pi
// (a script that reads stdin forever and emits nothing) via PI_BIN, connects as
// the node would, and verifies the ACP initialize handshake returns protocolVersion.
func TestAdapterBinarySpeaksACP(t *testing.T) {
	dir := t.TempDir()

	// Fake pi: ignore args, read stdin forever (so the launched process stays alive
	// and `initialize` succeeds). No rpc events are needed for the handshake.
	fakePi := filepath.Join(dir, "pi")
	if err := os.WriteFile(fakePi, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "pi-adapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cmd := exec.Command(bin)
	cmd.Env = append(cmd.Environ(),
		"ACP_LISTEN=tcp://"+addr,
		"PI_BIN="+fakePi,
		"PI_DIR="+dir,
		"SPAWN_MODEL=test-model",
	)
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

	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	if !strings.Contains(line, `"id":1`) || !strings.Contains(line, "protocolVersion") {
		t.Fatalf("unexpected initialize response: %s", line)
	}
}
