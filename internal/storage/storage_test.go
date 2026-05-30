package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScratchPrepareSeedsAndFinalizeNukes(t *testing.T) {
	root := t.TempDir()
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("secret"), 0o644)

	s := NewScratch(root)
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", seed)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// seeded file present in the prepared dir
	b, err := os.ReadFile(filepath.Join(hostDir, "README.md"))
	if err != nil || string(b) != "secret" {
		t.Fatalf("seed not copied: %q err=%v", b, err)
	}
	// finalize removes the dir
	if err := s.Finalize(context.Background(), hostDir); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := os.Stat(hostDir); !os.IsNotExist(err) {
		t.Fatalf("expected hostDir removed, stat err=%v", err)
	}
}

func TestScratchPrepareMissingSeedIsEmpty(t *testing.T) {
	s := NewScratch(t.TempDir())
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", "/no/such/seed")
	if err != nil {
		t.Fatalf("prepare with missing seed should succeed (empty mount): %v", err)
	}
	entries, _ := os.ReadDir(hostDir)
	if len(entries) != 0 {
		t.Fatalf("want empty mount, got %d entries", len(entries))
	}
}
