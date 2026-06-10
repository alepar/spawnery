package spawnlet

import (
	"context"
	"testing"
)

func TestNewManagerWithBackendInjectsBackendAndFloor(t *testing.T) {
	fb := &fakePodBackend{}
	fa := &fakeApplier{}
	m := NewManagerWithBackend(fb, fa, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true,
	})

	sp, err := m.Create(context.Background(), "spz", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sp.SandboxID != "sandbox-x" {
		t.Fatalf("SandboxID = %q, want sandbox-x (injected backend not used)", sp.SandboxID)
	}
	if !fa.applied {
		t.Fatal("injected firewall applier was not used on Create")
	}

	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fb.stopped == nil {
		t.Fatal("injected backend Stop not called")
	}
	if !fa.removed {
		t.Fatal("injected floor applier Remove not called on Stop")
	}
}

func TestNewManagerWithBackendAppliesDefaults(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	if m.cfg.SidecarPort != 8080 || m.cfg.MemLimitMB != 1024 || m.cfg.CPULimit != 1.0 || m.cfg.PidsLimit != 256 {
		t.Fatalf("defaults not applied: %+v", m.cfg)
	}
}
