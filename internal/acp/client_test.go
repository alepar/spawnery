package acp_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"spawnery/internal/acp"
	"spawnery/internal/stubagent"
)

func TestClientPromptAgainstStub(t *testing.T) {
	cliR, agentW := io.Pipe() // agent -> client
	agentR, cliW := io.Pipe() // client -> agent
	go stubagent.Run(agentR, agentW)

	c := acp.NewClient(cliR, cliW)
	if err := c.Initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.NewSession("/data"); err != nil {
		t.Fatalf("session: %v", err)
	}

	var got strings.Builder
	done := make(chan error, 1)
	go func() { done <- c.Prompt("hello", func(chunk string) { got.WriteString(chunk) }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
	if !strings.Contains(got.String(), "ECHO: hello") {
		t.Fatalf("got %q", got.String())
	}
}
