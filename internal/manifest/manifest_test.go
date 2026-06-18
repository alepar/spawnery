package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMounts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spawneryapp.yml"), []byte(`
apiVersion: spawnery/v1
kind: App
id: spawnery/secret
storage:
  mounts:
    - name: main
      path: data
      seed: seed
`), 0o644)

	m, err := Parse(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Storage.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(m.Storage.Mounts))
	}
	got := m.Storage.Mounts[0]
	if got.Name != "main" || got.Path != "data" || got.Seed != "seed" {
		t.Fatalf("mount mismatch: %+v", got)
	}
}

func TestParseFullSchema(t *testing.T) {
	m, err := Parse("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	if m.APIVersion != "spawnery/v1" || m.ID != "spawnery/secret-app" || m.Title != "Secret App" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Storage.Mounts) != 1 || m.Storage.Mounts[0].Name != "main" || m.Storage.Mounts[0].Path != "data" {
		t.Fatalf("mounts = %+v", m.Storage.Mounts)
	}
	if m.Visibility != "open" {
		t.Fatalf("visibility = %q", m.Visibility)
	}
}

func TestParseGitHubSlotMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spawneryapp.yml"), []byte(`
apiVersion: spawnery/v1
kind: App
id: spawnery/gh
storage:
  mounts:
    - name: repo
      path: repo
      durability: node-local
      github: true
    - name: cache
      path: cache
`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Parse(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Storage.Mounts) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(m.Storage.Mounts))
	}
	if !m.Storage.Mounts[0].Github {
		t.Fatalf("mount[0] github = false, want true")
	}
	if m.Storage.Mounts[1].Github {
		t.Fatalf("mount[1] github = true, want false (default)")
	}
}

func TestParseNoStorage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spawneryapp.yml"), []byte("id: spawnery/zork\n"), 0o644)
	m, err := Parse(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Storage.Mounts) != 0 {
		t.Fatalf("want 0 mounts, got %d", len(m.Storage.Mounts))
	}
}
