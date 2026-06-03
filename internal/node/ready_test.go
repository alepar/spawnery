package node

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAwaitInitialize_Ready(t *testing.T) {
	stdout := strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if !strings.Contains(stdin.String(), `"method":"initialize"`) {
		t.Fatalf("initialize not written to stdin: %q", stdin.String())
	}
}

func TestAwaitInitialize_IgnoresOtherFrames(t *testing.T) {
	// A notification (no id) before the matching response must be skipped, not mistaken for ready.
	stdout := strings.NewReader(
		`{"jsonrpc":"2.0","method":"session/update","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestAwaitInitialize_ErrorResponseCountsAsAnswered(t *testing.T) {
	// An error reply still proves the agent is up and answering — treat as ready.
	stdout := strings.NewReader(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"x"}}` + "\n")
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err != nil {
		t.Fatalf("want nil (agent answered), got %v", err)
	}
}

func TestAwaitInitialize_Timeout(t *testing.T) {
	pr, pw := io.Pipe() // never written, never closed -> the read blocks
	defer pw.Close()
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, pr, 50*time.Millisecond); err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestAwaitInitialize_StreamClosed(t *testing.T) {
	stdout := strings.NewReader("") // immediate EOF
	var stdin strings.Builder
	if err := awaitInitialize(context.Background(), &stdin, stdout, time.Second); err == nil {
		t.Fatal("want read error on EOF, got nil")
	}
}
