//go:build e2e

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

// TestTmuxRelayLiveAttach verifies a real tmux attach PTY through the relay by launching the agent
// image, attaching a client, and asserting terminal bytes arrive. Build-tagged e2e: it needs the
// docker CLI, a reachable daemon, and the built spawnery/agent:dev image — under the `e2e` tag those
// are preconditions, so a missing one FAILS (not skips); SKIP_DOCKER is the deliberate opt-out.
// Build the image first with `make .make/img-agent` (or `make images`).
func TestTmuxRelayLiveAttach(t *testing.T) {
	if os.Getenv("SKIP_DOCKER") != "" {
		t.Skip("SKIP_DOCKER set")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker not in PATH — required for this e2e test (set SKIP_DOCKER to opt out)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("docker daemon not reachable — required for this e2e test (set SKIP_DOCKER to opt out)")
	}
	if err := exec.Command("docker", "image", "inspect", "spawnery/agent:dev").Run(); err != nil {
		t.Fatalf("spawnery/agent:dev image not present — build it: make .make/img-agent")
	}

	// Launch the agent container via the dispatcher entrypoint (the runnable id; the image owns the
	// spawn-tmux wrapping — sp-9xr.13b).
	out, err := exec.Command("docker", "run", "-d", "--entrypoint", "/usr/bin/tini",
		"spawnery/agent:dev", "--", "/entrypoint.sh", "opencode-tui").Output()
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
