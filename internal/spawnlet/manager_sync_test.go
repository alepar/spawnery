package spawnlet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncMaterializedMountCopiesTreeWithoutFollowingSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	external := filepath.Join(t.TempDir(), "external.txt")

	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "child.txt"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("external-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(src, "link-out")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dst, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "old"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := syncMaterializedMount(src, dst); err != nil {
		t.Fatalf("syncMaterializedMount: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file should be removed, stat err=%v", err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "nested", "child.txt"))
	if err != nil || string(b) != "child" {
		t.Fatalf("nested child = %q err=%v, want child", b, err)
	}
	info, err := os.Lstat(filepath.Join(dst, "link-out"))
	if err != nil {
		t.Fatalf("Lstat link-out: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link-out mode=%v, want symlink", info.Mode())
	}
	target, err := os.Readlink(filepath.Join(dst, "link-out"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != external {
		t.Fatalf("link-out target=%q, want %q", target, external)
	}
}

func TestSyncMaterializedMountRejectsSourceDestinationEquality(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := syncMaterializedMount(dir, dir)
	if err == nil {
		t.Fatal("syncMaterializedMount should reject source/destination equality")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "same") {
		t.Fatalf("syncMaterializedMount error=%q, want same-path detail", err)
	}
	b, statErr := os.ReadFile(file)
	if statErr != nil || string(b) != "keep" {
		t.Fatalf("source tree should remain intact, got %q err=%v", b, statErr)
	}
}
