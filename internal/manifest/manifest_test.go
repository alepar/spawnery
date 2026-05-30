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
