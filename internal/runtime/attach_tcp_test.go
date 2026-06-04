package runtime

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestAttachTCPRoundtrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c) // echo
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	att, err := AttachTCP(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("AttachTCP: %v", err)
	}
	defer att.Close()

	if _, err := io.WriteString(att.Stdin, "ping\n"); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(att.Stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "ping\n" {
		t.Fatalf("got %q want %q", got, "ping\n")
	}
}

func TestAttachTCPTimesOutWhenUnreachable(t *testing.T) {
	// Reserve a port then close it so the dial is refused; a short ctx bounds the retry.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := AttachTCP(ctx, addr); err == nil {
		t.Fatal("expected error dialing a closed port")
	}
}
