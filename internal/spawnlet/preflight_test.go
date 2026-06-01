package spawnlet

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/runtime"
)

// rtErrOnRuntime errors when a non-default Runtime is requested (simulates broken/missing runsc).
type rtErrOnRuntime struct{ *runtime.FakeRuntime }

func (r rtErrOnRuntime) StartContainer(ctx context.Context, s runtime.ContainerSpec) (string, error) {
	if s.Runtime != "" {
		return "", errors.New("runsc not installed")
	}
	return r.FakeRuntime.StartContainer(ctx, s)
}

func TestPreflightRuntime(t *testing.T) {
	// no configured runtime -> no-op, nil
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	if err := m.PreflightRuntime(context.Background()); err != nil {
		t.Fatalf("empty runtime should preflight nil, got %v", err)
	}
	// configured runtime that works -> nil
	m2 := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), ContainerRuntime: "runsc"})
	if err := m2.PreflightRuntime(context.Background()); err != nil {
		t.Fatalf("healthy runtime should preflight nil, got %v", err)
	}
	// configured runtime that fails -> error
	m3 := NewManager(rtErrOnRuntime{runtime.NewFake()}, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), ContainerRuntime: "runsc"})
	if err := m3.PreflightRuntime(context.Background()); err == nil {
		t.Fatal("broken runtime must fail preflight")
	}
}
