package node

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

func TestRunRetriesUntilContextCancelled(t *testing.T) {
	mgr := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	cfg := Config{NodeID: "node-1", CPURL: "http://127.0.0.1:1", MaxSpawns: 1} // :1 = unreachable

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Run(ctx, mgr, http.DefaultClient, cfg)
	elapsed := time.Since(start)

	// Run must NOT exit on the first connection failure — it retries until ctx is cancelled, then
	// returns a context error (NOT the connection-refused error).
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want a context error (it should retry, not bail on conn failure)", err)
	}
	// It should have stuck around until ~the ctx deadline (retrying), not returned at t≈0.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("Run returned after %s — looks like it exited on the first failure instead of retrying", elapsed)
	}
}
