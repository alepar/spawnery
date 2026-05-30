package spawnlet

import (
	"context"
	"os"
	"testing"

	"connectrpc.com/connect"
	spawnv1 "spawnery/gen/spawn/v1"
	"spawnery/internal/runtime"
)

func TestServerCreateSpawn(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	srv := NewServer(m)

	app := t.TempDir()
	os.WriteFile(app+"/spawneryapp.yml", []byte("id: t/a\n"), 0o644)

	resp, err := srv.CreateSpawn(context.Background(), connect.NewRequest(&spawnv1.CreateSpawnRequest{
		AppPath: app, Model: "anthropic/claude-3.5-sonnet",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Msg.SpawnId == "" {
		t.Fatal("empty spawn id")
	}
	if _, ok := m.Store().Get(resp.Msg.SpawnId); !ok {
		t.Fatal("spawn not stored")
	}
}
