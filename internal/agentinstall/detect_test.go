package agentinstall_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestDetectEmpty(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	got := agentinstall.Detect(env)
	if len(got) != 0 {
		t.Errorf("expected no detected agents, got %v", got)
	}
}

func TestDetectClaudePresent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{"HOME": home}
	got := agentinstall.Detect(env)
	if len(got) != 1 || got[0] != "claude" {
		t.Errorf("expected [claude], got %v", got)
	}
}

func TestDetectCodexViaCodexHome(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	// codexHome itself must exist (it's a temp dir, so it does)
	env := agentinstall.MapEnviron{
		"HOME":       home,
		"CODEX_HOME": codexHome,
	}
	got := agentinstall.Detect(env)
	if len(got) != 1 || got[0] != "codex" {
		t.Errorf("expected [codex], got %v", got)
	}
}

func TestDetectCodexDefault(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{"HOME": home}
	got := agentinstall.Detect(env)
	if len(got) != 1 || got[0] != "codex" {
		t.Errorf("expected [codex], got %v", got)
	}
}

func TestDetectXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	// Create opencode and goose roots under xdg
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "goose"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{
		"HOME":            home,
		"XDG_CONFIG_HOME": xdg,
	}
	got := agentinstall.Detect(env)
	sort.Strings(got)
	want := []string{"goose", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetectAll(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".claude", ".codex", ".hermes"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "goose"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{"HOME": home}
	got := agentinstall.Detect(env)
	// Should be in canonical order
	want := []string{"claude", "codex", "opencode", "hermes", "goose"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestDetectHermesPresent(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := agentinstall.MapEnviron{"HOME": home}
	got := agentinstall.Detect(env)
	if len(got) != 1 || got[0] != "hermes" {
		t.Errorf("expected [hermes], got %v", got)
	}
}
