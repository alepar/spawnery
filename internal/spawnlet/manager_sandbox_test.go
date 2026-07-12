package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

func TestManagerThreadsSandboxID(t *testing.T) {
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	fb := fakeBackend(t)
	m.pod = fb // white-box: replace the Docker backend with the fake

	sp, err := m.Create(context.Background(), "spx", "../../examples/secret-app", "model", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sp.SandboxID != "spx-sandbox" {
		t.Fatalf("Spawn.SandboxID = %q, want spx-sandbox", sp.SandboxID)
	}
	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatal(err)
	}
	h := fb.LastStopHandle()
	if h == nil || h.SandboxID != "spx-sandbox" {
		t.Fatalf("Stop handle SandboxID = %+v, want spx-sandbox", h)
	}
}

// captureHookBackend wraps the shared fakepod.Backend to run captureHook at the top of
// CaptureDelta. Lets a test observe host state AT CAPTURE TIME — the mount host dirs are removed
// by Scratch.Finalize later in teardown, so a post-Suspend assertion about mount contents would
// always fail (sp-2tx8.2.2).
type captureHookBackend struct {
	*fakepod.Backend
	captureHook func()
}

func (b *captureHookBackend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	if b.captureHook != nil {
		b.captureHook()
	}
	return b.Backend.CaptureDelta(ctx, h)
}
