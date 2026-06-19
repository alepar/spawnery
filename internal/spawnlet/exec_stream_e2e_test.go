//go:build e2e

package spawnlet_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
)

// TestExecStream_RealDockerExec drives Manager.ExecStream against a real (stubagent) container over the
// Docker lane (sp-8v39): it proves a command's stdout and stderr arrive separated and its exit code is
// propagated. Credential-free — the stub needs no model key. Build-tagged e2e: Docker and the
// spawnery/{stubagent,sidecar}:dev images are preconditions under the tag, so their absence FAILS.
func TestExecStream_RealDockerExec(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable (required for this e2e; run `make images` + have docker): %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}

	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage:    "spawnery/stubagent:dev",
		SidecarImage:  "spawnery/sidecar:dev",
		OpenRouterKey: "unused",
		DataRoot:      t.TempDir(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sp, err := mgr.Create(ctx, "exec-e2e", mustAbs(t, "../../examples/secret-app"), "x", "", "", 0)
	if err != nil {
		t.Fatalf("create spawn: %v", err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		_ = mgr.Stop(stopCtx, sp.ID)
	}()

	var stdout, stderr bytes.Buffer
	code, err := mgr.ExecStream(ctx, sp.ID,
		[]string{"sh", "-c", "printf out; printf err 1>&2; exit 7"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if stdout.String() != "out" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), "out")
	}
	if stderr.String() != "err" {
		t.Fatalf("stderr = %q, want %q", stderr.String(), "err")
	}
}
