//go:build e2e

package spawnlet

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestStartTerminal_RealMoshServer exercises the real mosh-server launch + parse path with a
// harmless child command (echo), proving StartTerminal returns valid connect info. Build-tagged
// e2e (mosh lane): mosh-server is a precondition under the tag, so its absence FAILS, not skips.
func TestStartTerminal_RealMoshServer(t *testing.T) {
	if _, err := exec.LookPath("mosh-server"); err != nil {
		t.Fatalf("mosh-server not installed — required for this e2e test (install mosh)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ExecPrefix=["/bin/echo"] => child is `/bin/echo <id> tmux new-session ...`: mosh-server prints
	// MOSH CONNECT then runs it. We only assert the connect info is produced.
	ts, err := StartTerminal(ctx, "container-x", nil, TerminalConfig{
		ExecPrefix:  []string{"/bin/echo"},
		AdvertiseIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("StartTerminal: %v", err)
	}
	if ts.Port <= 0 || ts.Key == "" {
		t.Fatalf("bad terminal session: %+v", ts)
	}
	if ts.Host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1", ts.Host)
	}
}
