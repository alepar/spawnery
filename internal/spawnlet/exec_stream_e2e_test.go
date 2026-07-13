//go:build e2e

package spawnlet_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
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

func TestExecStreamCancellationKillsRealDockerCommand(t *testing.T) {
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable (required for this e2e; run `make images` + have docker): %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}
	mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{
		AgentImage: "spawnery/stubagent:dev", SidecarImage: "spawnery/sidecar:dev",
		OpenRouterKey: "unused", DataRoot: t.TempDir(),
	})
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	sp, err := mgr.Create(ctx, "exec-cancel-e2e", mustAbs(t, "../../examples/secret-app"), "x", "", "", 0)
	if err != nil {
		t.Fatalf("create spawn: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = mgr.Stop(stopCtx, sp.ID)
	}()

	const pidFile = "/tmp/spawnery-cancel-target.pid"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.ExecRun(cleanupCtx, sp.ID, []string{"sh", "-c", `test ! -s "$1" || kill -KILL "$(cat "$1")" 2>/dev/null || true`, "sh", pidFile})
	})
	execCtx, cancelExec := context.WithCancel(ctx)
	ready := &signalWriter{needle: "ready", found: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := mgr.ExecStream(execCtx, sp.ID,
			[]string{"sh", "-c", "echo $$ > \"$1\"; echo ready; exec sleep 600", "sh", pidFile},
			ready, io.Discard)
		done <- err
	}()
	select {
	case <-ready.found:
	case <-ctx.Done():
		t.Fatal("in-container command did not start")
	}
	cancelExec()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecStream error = %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("ExecStream did not return after cancellation")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := mgr.ExecRun(probeCtx, sp.ID, []string{
			"sh", "-c", "pid=$(cat \"$1\"); test ! -e \"/proc/$pid\"", "sh", pidFile,
		})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-container command remains alive after ExecStream cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type signalWriter struct {
	mu     sync.Mutex
	needle string
	found  chan struct{}
	once   sync.Once
	buf    strings.Builder
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	found := strings.Contains(w.buf.String(), w.needle)
	w.mu.Unlock()
	if found {
		w.once.Do(func() { close(w.found) })
	}
	return len(p), nil
}
