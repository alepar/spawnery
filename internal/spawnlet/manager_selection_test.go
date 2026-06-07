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

func TestCreateWithSelectionAcpUsesImageNoCmd(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-sel", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "goose-acp", Mode: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if fb.agentSpec.Image != "selected:img" {
		t.Fatalf("agent image = %q, want selected:img", fb.agentSpec.Image)
	}
	if fb.agentSpec.Cmd != nil {
		t.Fatalf("acp mode should NOT override Cmd (entrypoint runs the adapter), got %v", fb.agentSpec.Cmd)
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

func TestCreateWithSelectionTmuxWrapsInTmux(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-tmux", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "opencode-tui", Mode: "tmux"})
	if err != nil {
		t.Fatalf("tmux mode should now launch, got error: %v", err)
	}
	cmd := fb.agentSpec.Cmd
	if len(cmd) < 2 || cmd[0] != "spawn-tmux" || cmd[1] != "opencode" {
		t.Fatalf("tmux launch cmd = %v, want [spawn-tmux opencode]", cmd)
	}
}
