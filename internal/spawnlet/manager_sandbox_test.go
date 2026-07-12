package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
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
