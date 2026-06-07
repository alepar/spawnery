package node

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseClientFrame(t *testing.T) {
	// input opcode
	kind, data, cols, rows := parseClientFrame([]byte{tmuxOpInput, 'h', 'i'})
	if kind != tmuxOpInput || string(data) != "hi" {
		t.Fatalf("input: kind=%d data=%q", kind, data)
	}
	// resize opcode
	kind, _, cols, rows = parseClientFrame(append([]byte{tmuxOpResize}, []byte("120 40")...))
	if kind != tmuxOpResize || cols != 120 || rows != 40 {
		t.Fatalf("resize: kind=%d cols=%d rows=%d", kind, cols, rows)
	}
	// empty frame → treated as input (no-op), never panics
	if k, _, _, _ := parseClientFrame(nil); k != tmuxOpInput {
		t.Fatalf("empty frame should default to input")
	}
}

// TestTmuxRelayLiveAttach verifies a real tmux attach PTY through the relay by launching the .5
// image, attaching a client, and asserting terminal bytes arrive. Requires Docker and the
// spawnery/agent:dev image; skipped when SKIP_DOCKER is set.
func TestTmuxRelayLiveAttach(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER set")
	}
	// Check Docker is available.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH")
	}

	// Launch the agent container with the spawn-tmux wrapper (mirrors what .5 does).
	out, err := exec.Command("docker", "run", "-d", "--entrypoint", "/usr/bin/tini",
		"spawnery/agent:dev", "--", "/entrypoint.sh", "/usr/local/bin/spawn-tmux", "opencode").Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	cid := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })

	// Wait for the tmux session to come up (spawn-tmux launches in background).
	time.Sleep(4 * time.Second)

	// Build the attach argv exactly as the relay would (docker exec -it ... tmux attach -t spawn).
	attachArgv := []string{"docker", "exec", "-it", "-e", "TERM=xterm-256color", cid, "tmux", "attach", "-t", "spawn"}

	var mu sync.Mutex
	var received []byte
	gotBytes := make(chan struct{}, 1)

	relay := newTmuxRelay(attachArgv, func(clientID string, data []byte) error {
		mu.Lock()
		received = append(received, data...)
		mu.Unlock()
		select {
		case gotBytes <- struct{}{}:
		default:
		}
		return nil
	})

	if err := relay.attach(context.Background(), "c1"); err != nil {
		t.Fatalf("relay.attach: %v", err)
	}

	// Wait for terminal bytes to arrive (the TUI starts rendering quickly).
	select {
	case <-gotBytes:
		// success: terminal output received
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for terminal bytes from relay")
	}

	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n == 0 {
		t.Fatal("received 0 bytes from relay; expected terminal output")
	}
	t.Logf("relay received %d terminal bytes from tmux attach PTY", n)

	relay.stop()
}
