package spawnlet

import (
	"context"
	"os"
	"testing"

	"spawnery/internal/runtime"
)

func TestCreateStartsSidecarThenAgentJoiningNetns(t *testing.T) {
	f := runtime.NewFake()
	dataRoot := t.TempDir()
	m := NewManager(f, ManagerConfig{
		AgentImage: "agent", SidecarImage: "sidecar",
		OpenRouterKey: "k", DataRoot: dataRoot,
	})
	app := t.TempDir()
	os.WriteFile(app+"/spawneryapp.yml", []byte("id: test/app\n"), 0o644)

	sp, err := m.Create(context.Background(), "test-1", app, "", "anthropic/claude-3.5-sonnet")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(f.Started) != 2 {
		t.Fatalf("want 2 containers, got %d", len(f.Started))
	}
	sidecar, agent := f.Started[0], f.Started[1]
	if agent.NetnsOf != sp.SidecarID {
		t.Fatalf("agent should join sidecar netns, got %q want %q", agent.NetnsOf, sp.SidecarID)
	}
	if !hasEnv(sidecar.Env, "OPENROUTER_API_KEY=k") {
		t.Fatalf("sidecar missing key env: %v", sidecar.Env)
	}
	if !hasMountRO(agent.Mounts, "/app") || !hasMountRW(agent.Mounts, "/data") {
		t.Fatalf("agent mounts wrong: %+v", agent.Mounts)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
func hasMountRO(ms []runtime.Mount, cp string) bool {
	for _, m := range ms {
		if m.ContainerPath == cp && m.ReadOnly {
			return true
		}
	}
	return false
}
func hasMountRW(ms []runtime.Mount, cp string) bool {
	for _, m := range ms {
		if m.ContainerPath == cp && !m.ReadOnly {
			return true
		}
	}
	return false
}
