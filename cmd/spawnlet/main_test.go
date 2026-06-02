package main

import (
	"testing"

	"spawnery/internal/spawnlet"
)

func TestBuildManagerRunscPath(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		ContainerRuntime: "runsc", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runsc buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestBuildManagerDockerDefault(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("docker buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}
