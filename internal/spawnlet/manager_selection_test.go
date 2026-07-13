package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

func newSelManager(t *testing.T) (*Manager, *fakepod.Backend) {
	t.Helper()
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "cfg-default:img", SidecarImage: "s", DataRoot: t.TempDir()})
	fb := fakeBackend(t)
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
	if fb.AgentSpec("sp-sel").Image != "selected:img" {
		t.Fatalf("agent image = %q, want selected:img", fb.AgentSpec("sp-sel").Image)
	}
	// Any runnable selection (including acp/served) now yields Cmd=[runnableID]; the image's
	// dispatcher entrypoint resolves the actual launch (sp-9xr.13b).
	if len(fb.AgentSpec("sp-sel").Cmd) != 1 || fb.AgentSpec("sp-sel").Cmd[0] != "goose-acp" {
		t.Fatalf("acp selection should yield Cmd=[\"goose-acp\"], got %v", fb.AgentSpec("sp-sel").Cmd)
	}
}

func TestCreateLegacyUsesConfiguredImageNoCmd(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.Create(context.Background(), "sp-legacy", "../../examples/secret-app", "model", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb.AgentSpec("sp-legacy").Image != "cfg-default:img" {
		t.Fatalf("agent image = %q, want cfg-default:img", fb.AgentSpec("sp-legacy").Image)
	}
	if fb.AgentSpec("sp-legacy").Cmd != nil {
		t.Fatalf("legacy cmd should be nil, got %v", fb.AgentSpec("sp-legacy").Cmd)
	}
}

func TestCreateWithSelectionTmuxPassesRunnableID(t *testing.T) {
	m, fb := newSelManager(t)
	_, err := m.CreateWithSelection(context.Background(), "sp-tmux", "../../examples/secret-app", "model", "", "", 0,
		AgentSelection{Image: "selected:img", RunnableID: "opencode-tui", Mode: "tmux"})
	if err != nil {
		t.Fatalf("tmux mode should launch, got error: %v", err)
	}
	cmd := fb.AgentSpec("sp-tmux").Cmd
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
