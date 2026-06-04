package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/runtime"
)

func writeApp(t *testing.T) string {
	t.Helper()
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "spawneryapp.yml"), []byte(`
id: spawnery/secret
storage:
  mounts:
    - name: main
      path: data
      seed: seed
`), 0o644)
	os.MkdirAll(filepath.Join(app, "seed"), 0o755)
	os.WriteFile(filepath.Join(app, "seed", "README.md"), []byte("QUOKKA-4417"), 0o644)
	return app
}

func TestCreateMountsAppRoAndNamedDataRw(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	app := writeApp(t)

	sp, err := m.Create(context.Background(), "test-1", app, "model-x", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(f.Started) != 2 {
		t.Fatalf("want 2 containers, got %d", len(f.Started))
	}
	agent := f.Started[1]
	if agent.NetnsOf != sp.SidecarID {
		t.Fatalf("agent should join sidecar netns")
	}
	if !hasMountRO(agent.Mounts, "/app") {
		t.Fatalf("/app should be ro: %+v", agent.Mounts)
	}
	if !hasMountRW(agent.Mounts, "/app/data") {
		t.Fatalf("/app/data should be rw: %+v", agent.Mounts)
	}
	// the rw mount's host dir was seeded
	if len(sp.MountDirs) != 1 {
		t.Fatalf("want 1 mount dir, got %d", len(sp.MountDirs))
	}
	b, err := os.ReadFile(filepath.Join(sp.MountDirs[0], "README.md"))
	if err != nil || string(b) != "QUOKKA-4417" {
		t.Fatalf("mount not seeded: %q err=%v", b, err)
	}
}

func TestStopFinalizesMounts(t *testing.T) {
	f := runtime.NewFake()
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	sp, err := m.Create(context.Background(), "test-2", writeApp(t), "model-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := sp.MountDirs[0]
	if err := m.Stop(context.Background(), "test-2"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch mount should be nuked on stop, stat err=%v", err)
	}
}

func TestCreateRecordsNetnsPath(t *testing.T) {
	f := runtime.NewFake() // FakeRuntime.ContainerPID returns 4242
	m := NewManager(f, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	sp, err := m.Create(context.Background(), "n-1", writeApp(t), "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sp.NetnsPath != "/proc/4242/ns/net" {
		t.Fatalf("NetnsPath = %q, want /proc/4242/ns/net", sp.NetnsPath)
	}
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
