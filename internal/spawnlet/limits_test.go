package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

func TestCreateAppliesResourceLimits(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		MemLimitMB: 512, CPULimit: 2.0, PidsLimit: 128, ContainerRuntime: "runsc",
	})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model", "", "", 0); err != nil {
		t.Fatal(err)
	}
	if len(rt.Started) != 2 {
		t.Fatalf("want sidecar+agent started, got %d", len(rt.Started))
	}
	for i, spec := range rt.Started {
		if spec.MemoryBytes != 512<<20 {
			t.Fatalf("spec[%d] Memory = %d (want %d)", i, spec.MemoryBytes, 512<<20)
		}
		if spec.NanoCPUs != 2_000_000_000 {
			t.Fatalf("spec[%d] NanoCPUs = %d", i, spec.NanoCPUs)
		}
		if spec.PidsLimit != 128 {
			t.Fatalf("spec[%d] PidsLimit = %d", i, spec.PidsLimit)
		}
		if spec.Runtime != "runsc" {
			t.Fatalf("spec[%d] Runtime = %q", i, spec.Runtime)
		}
	}
}

func TestNewManagerLimitDefaults(t *testing.T) {
	rt := runtime.NewFake()
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model", "", "", 0); err != nil {
		t.Fatal(err)
	}
	s := rt.Started[0]
	if s.MemoryBytes != 1024<<20 || s.NanoCPUs != 1_000_000_000 || s.PidsLimit != 256 || s.Runtime != "" {
		t.Fatalf("default limits wrong: mem=%d cpu=%d pids=%d rt=%q", s.MemoryBytes, s.NanoCPUs, s.PidsLimit, s.Runtime)
	}
}
