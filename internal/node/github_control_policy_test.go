package node

import (
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage"
)

func TestConfigureGitHubControlFollowsCredentialLane(t *testing.T) {
	mint := &fakeMintClient{}
	refresher := newGitHubRefresher(mint)

	staticManager := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{
		AgentImage:   "agent",
		SidecarImage: "sidecar",
		DataRoot:     t.TempDir(),
		GitHubStaticCredentials: storage.StaticGitHubCredentials{
			AccessToken: "static-token",
		},
	})
	if got := configureGitHubControl(staticManager, mint, refresher); got != nil {
		t.Fatal("static credential lane installed JIT GitHub control")
	}
	if staticManager.GitHubControlEnabled() {
		t.Fatal("static credential lane enabled the JIT proxy/control path")
	}

	dynamicManager := spawnlet.NewManager(runtime.NewFake(), spawnlet.ManagerConfig{
		AgentImage:   "agent",
		SidecarImage: "sidecar",
		DataRoot:     t.TempDir(),
	})
	if got := configureGitHubControl(dynamicManager, mint, refresher); got == nil {
		t.Fatal("dynamic credential lane omitted JIT GitHub control")
	}
	if !dynamicManager.GitHubControlEnabled() {
		t.Fatal("dynamic credential lane did not enable the JIT proxy/control path")
	}
}
