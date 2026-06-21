package config

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/knadh/koanf/v2"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"common.yaml":      {Data: []byte("a: 1\nnested:\n  x: 1\n  y: 1\n")},
		"common.prod.yaml": {Data: []byte("nested:\n  y: 2\n")},
		"cp.yaml":          {Data: []byte("b: 1\nlist:\n  - 1\n  - 2\n")},
		"cp.prod.yaml":     {Data: []byte("b: 2\n")},
	}
}

func TestLoadFiles_LayersAndDeepMerge(t *testing.T) {
	k := koanf.New(".")
	if err := loadFiles(k, "cp", "prod", testFS(), ""); err != nil {
		t.Fatalf("loadFiles: %v", err)
	}
	if got := k.Int("a"); got != 1 {
		t.Errorf("a = %d, want 1 (common base value should survive)", got)
	}
	if got := k.Int("nested.x"); got != 1 {
		t.Errorf("nested.x = %d, want 1 (deep-merge must preserve sibling)", got)
	}
	if got := k.Int("nested.y"); got != 2 {
		t.Errorf("nested.y = %d, want 2 (common.prod overrides common)", got)
	}
	if got := k.Int("b"); got != 2 {
		t.Errorf("b = %d, want 2 (cp.prod overrides cp)", got)
	}
}

func TestLoadFiles_OptionalEnvDeltaAbsentIsFine(t *testing.T) {
	// dev env: no common.dev.yaml / cp.dev.yaml in the FS — must load base files without error.
	k := koanf.New(".")
	if err := loadFiles(k, "cp", "dev", testFS(), ""); err != nil {
		t.Fatalf("loadFiles dev (no deltas): %v", err)
	}
	if got := k.Int("b"); got != 1 {
		t.Errorf("b = %d, want 1 (cp base only)", got)
	}
}

func TestLoadFiles_MissingBaseIsFatal(t *testing.T) {
	fsys := testFS()
	delete(fsys, "common.yaml") // a required base file
	k := koanf.New(".")
	if err := loadFiles(k, "cp", "prod", fsys, ""); err == nil {
		t.Fatal("expected error when required base common.yaml is absent, got nil")
	}
}

func TestLoadFiles_ExternalDirOverlays(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cp.prod.yaml"), []byte("b: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := koanf.New(".")
	if err := loadFiles(k, "cp", "prod", testFS(), dir); err != nil {
		t.Fatalf("loadFiles: %v", err)
	}
	if got := k.Int("b"); got != 99 {
		t.Errorf("b = %d, want 99 (external cp.prod.yaml overlays embedded)", got)
	}
	if got := k.Int("a"); got != 1 {
		t.Errorf("a = %d, want 1 (external overlay must not drop embedded common values)", got)
	}
}
