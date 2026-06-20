package spawnlet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/runtime"
)

// TestRenderGitHubIdentitySeedsGitEnvGitconfig verifies that RenderGitHubIdentity writes the seeded
// [user] identity into the spawn's git-env gitconfig under the manager's git-env root.
func TestRenderGitHubIdentitySeedsGitEnvGitconfig(t *testing.T) {
	dataRoot := t.TempDir()
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot})

	// Canonical: login + userID both present.
	if err := m.RenderGitHubIdentity("sp1", "octocat", 583231); err != nil {
		t.Fatalf("RenderGitHubIdentity canonical: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dataRoot, "git-env", "sp1", GitConfigName))
	if err != nil {
		t.Fatalf("ReadFile canonical: %v", err)
	}
	if !strings.Contains(string(b), "name = octocat") {
		t.Errorf("canonical: gitconfig missing 'name = octocat': %q", string(b))
	}
	if !strings.Contains(string(b), "583231+octocat@users.noreply.github.com") {
		t.Errorf("canonical: gitconfig missing canonical email: %q", string(b))
	}

	// Fallback: login empty, userID 0 => "spawnery" / spawnery.local.
	if err := m.RenderGitHubIdentity("sp2", "", 0); err != nil {
		t.Fatalf("RenderGitHubIdentity fallback: %v", err)
	}
	b2, err := os.ReadFile(filepath.Join(dataRoot, "git-env", "sp2", GitConfigName))
	if err != nil {
		t.Fatalf("ReadFile fallback: %v", err)
	}
	if !strings.Contains(string(b2), "name = spawnery") {
		t.Errorf("fallback: gitconfig missing 'name = spawnery': %q", string(b2))
	}
	if !strings.Contains(string(b2), "spawnery@users.noreply.spawnery.local") {
		t.Errorf("fallback: gitconfig missing fallback email: %q", string(b2))
	}
}
