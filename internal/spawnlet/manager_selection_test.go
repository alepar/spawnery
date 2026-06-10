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

func TestCreateWithSelectionAcpUsesRunnableID(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-sel", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "goose-acp", Mode: "acp"})
	if err != nil {
		t.Fatal(err)
	}
	if fb.agentSpec.Image != "selected:img" {
		t.Fatalf("agent image = %q, want selected:img", fb.agentSpec.Image)
	}
	// Any runnable selection (including acp/served) now yields Cmd=[runnableID]; the image's
	// dispatcher entrypoint resolves the actual launch (sp-9xr.13b).
	if len(fb.agentSpec.Cmd) != 1 || fb.agentSpec.Cmd[0] != "goose-acp" {
		t.Fatalf("acp selection should yield Cmd=[\"goose-acp\"], got %v", fb.agentSpec.Cmd)
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

func TestCreateWithSelectionTmuxPassesRunnableID(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-tmux", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "opencode-tui", Mode: "tmux"})
	if err != nil {
		t.Fatalf("tmux mode should launch, got error: %v", err)
	}
	cmd := fb.agentSpec.Cmd
	// The dispatcher (image entrypoint) now owns tmux-wrapping; node just passes the runnable id.
	if len(cmd) != 1 || cmd[0] != "opencode-tui" {
		t.Fatalf("tmux launch cmd = %v, want [\"opencode-tui\"]", cmd)
	}
}

func TestCreateWithSelectionUnknownRunnableErrors(t *testing.T) {
	m, _ := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-bad", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "does-not-exist", Mode: "tmux"})
	if err == nil {
		t.Fatal("unknown runnable should return error")
	}
}
