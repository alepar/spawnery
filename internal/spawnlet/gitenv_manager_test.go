package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spawnery/internal/runtime"
)

// TestCreateThreadsGitEnvMountAndEnv verifies that CreateWithSelection binds a per-spawn git-env
// dir at GitEnvMountPath and injects GIT_CONFIG_GLOBAL + the three hardening vars into AgentSpec.
// Modelled on TestManagerArtifactsMaterialized in artifacts_test.go.
func TestCreateThreadsGitEnvMountAndEnv(t *testing.T) {
	dataRoot := t.TempDir()
	m := NewManager(runtime.NewFake(), ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: dataRoot,
	})
	fb := &fakePodBackend{}
	m.pod = fb

	spawnID := "sp-gitenv"
	_, err := m.CreateWithSelection(context.Background(), spawnID, "../../examples/secret-app", "model", "", "", 0, AgentSelection{})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}

	// (a) git-env bind-mount: correct ContainerPath and HostPath, and the host dir exists.
	wantHostPath := filepath.Join(dataRoot, "git-env", spawnID)
	var foundMount bool
	for _, mt := range fb.agentSpec.Mounts {
		if mt.ContainerPath == GitEnvMountPath {
			if mt.HostPath != wantHostPath {
				t.Fatalf("git-env mount HostPath = %q, want %q", mt.HostPath, wantHostPath)
			}
			foundMount = true
			break
		}
	}
	if !foundMount {
		t.Fatalf("GitEnvMountPath %q not found in agent mounts: %+v", GitEnvMountPath, fb.agentSpec.Mounts)
	}
	if _, err := os.Stat(wantHostPath); err != nil {
		t.Fatalf("git-env host dir %s does not exist: %v", wantHostPath, err)
	}

	// (b) agent env contains the four git env vars.
	envSet := make(map[string]string, len(fb.agentSpec.Env))
	for _, kv := range fb.agentSpec.Env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			envSet[kv[:i]] = kv[i+1:]
		}
	}
	wantGitConfig := GitEnvMountPath + "/" + GitConfigName
	if got := envSet["GIT_CONFIG_GLOBAL"]; got != wantGitConfig {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want %q", got, wantGitConfig)
	}
	for _, key := range []string{"GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS"} {
		if _, ok := envSet[key]; !ok {
			t.Fatalf("env var %s missing from AgentSpec.Env", key)
		}
	}
}
