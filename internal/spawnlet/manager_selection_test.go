package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

func newSelManager(t *testing.T) (*Manager, *fakePodBackend) {
	t.Helper()
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "cfg-default:img", SidecarImage: "s", DataRoot: t.TempDir()})
	fb := &fakePodBackend{}
	m.pod = fb
	return m, fb
}

func TestCreateWithSelectionUsesImageAndCmd(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-sel", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "goose-acp", Mode: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if fb.agentSpec.Image != "selected:img" {
		t.Fatalf("agent image = %q, want selected:img", fb.agentSpec.Image)
	}
	if len(fb.agentSpec.Cmd) != 2 || fb.agentSpec.Cmd[0] != "goose" || fb.agentSpec.Cmd[1] != "acp" {
		t.Fatalf("agent cmd = %v, want [goose acp]", fb.agentSpec.Cmd)
	}
}

func TestCreateLegacyUsesConfiguredImageNoCmd(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.Create(context.Background(), "sp-legacy", "../../examples/secret-app", "model", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb.agentSpec.Image != "cfg-default:img" {
		t.Fatalf("agent image = %q, want cfg-default:img", fb.agentSpec.Image)
	}
	if fb.agentSpec.Cmd != nil {
		t.Fatalf("legacy cmd should be nil, got %v", fb.agentSpec.Cmd)
	}
}

func TestCreateWithSelectionRejectsTmux(t *testing.T) {
	m, _ := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-tmux", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "goose-tui", Mode: "tmux"})
	if err == nil {
		t.Fatal("expected error for tmux mode at node")
	}
}
