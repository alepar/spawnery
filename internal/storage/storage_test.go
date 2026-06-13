package storage

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestScratchPrepareSeedsAndFinalizeNukes(t *testing.T) {
	root := t.TempDir()
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("secret"), 0o644)

	s := NewScratch(root)
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", seed, os.Getuid())
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
	hostDir, err := s.Prepare(context.Background(), "spawn1", "main", "/no/such/seed", os.Getuid())
	if err != nil {
		t.Fatalf("prepare with missing seed should succeed (empty mount): %v", err)
	}
	entries, _ := os.ReadDir(hostDir)
	if len(entries) != 0 {
		t.Fatalf("want empty mount, got %d entries", len(entries))
	}
}

// TestScratchPrepareChownsIntoRangeAndPerms verifies that a non-degraded agentUID
// results in 0755 dir + 0644 seed file owned by agentUID.
func TestScratchPrepareChownsIntoRangeAndPerms(t *testing.T) {
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "config.txt"), []byte("hello"), 0o644)

	s := NewScratch(t.TempDir())
	uid := os.Getuid()
	hostDir, err := s.Prepare(context.Background(), "spawn2", "data", seed, uid)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Check dir permissions
	info, err := os.Stat(hostDir)
	if err != nil {
		t.Fatalf("stat hostDir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("hostDir perm: got %04o, want 0755", perm)
	}
	// Check dir ownership
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != uid {
			t.Errorf("hostDir uid: got %d, want %d", st.Uid, uid)
		}
	}

	// Check seeded file permissions
	finfo, err := os.Stat(filepath.Join(hostDir, "config.txt"))
	if err != nil {
		t.Fatalf("stat seed file: %v", err)
	}
	if perm := finfo.Mode().Perm(); perm != 0o644 {
		t.Errorf("seed file perm: got %04o, want 0644", perm)
	}
	// Check seed file ownership
	if st, ok := finfo.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != uid {
			t.Errorf("seed file uid: got %d, want %d", st.Uid, uid)
		}
	}
}

// TestScratchPrepareDegradedKeeps0777 verifies that agentUID=-1 (degraded lane) yields
// world-writable perms and no chown is attempted.
func TestScratchPrepareDegradedKeeps0777(t *testing.T) {
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "note.txt"), []byte("data"), 0o644)

	chownCalled := false
	orig := osChown
	osChown = func(name string, uid, gid int) error {
		chownCalled = true
		return orig(name, uid, gid)
	}
	defer func() { osChown = orig }()

	s := NewScratch(t.TempDir())
	hostDir, err := s.Prepare(context.Background(), "spawn3", "data", seed, -1)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if chownCalled {
		t.Error("chown should NOT be called in the degraded lane (agentUID=-1)")
	}

	// dir should be 0777
	info, err := os.Stat(hostDir)
	if err != nil {
		t.Fatalf("stat hostDir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o777 {
		t.Errorf("hostDir perm: got %04o, want 0777", perm)
	}

	// seeded file should be 0666
	finfo, err := os.Stat(filepath.Join(hostDir, "note.txt"))
	if err != nil {
		t.Fatalf("stat seed file: %v", err)
	}
	if perm := finfo.Mode().Perm(); perm != 0o666 {
		t.Errorf("seed file perm: got %04o, want 0666", perm)
	}
}

// TestScratchPrepareEPERMFallback simulates a chown that returns EPERM and verifies that
// Prepare falls back to degraded (world-writable) perms rather than erroring.
func TestScratchPrepareEPERMFallback(t *testing.T) {
	seed := t.TempDir()
	os.WriteFile(filepath.Join(seed, "seed.txt"), []byte("content"), 0o644)

	orig := osChown
	osChown = func(name string, uid, gid int) error {
		return &os.PathError{Op: "chown", Path: name, Err: syscall.EPERM}
	}
	defer func() { osChown = orig }()

	s := NewScratch(t.TempDir())
	hostDir, err := s.Prepare(context.Background(), "spawn4", "data", seed, 4242)
	if err != nil {
		t.Fatalf("prepare should succeed after EPERM fallback, got: %v", err)
	}

	// dir should fall back to 0777
	info, err := os.Stat(hostDir)
	if err != nil {
		t.Fatalf("stat hostDir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o777 {
		t.Errorf("hostDir perm after EPERM: got %04o, want 0777", perm)
	}

	// seeded file should be 0666
	finfo, err := os.Stat(filepath.Join(hostDir, "seed.txt"))
	if err != nil {
		t.Fatalf("stat seed file: %v", err)
	}
	if perm := finfo.Mode().Perm(); perm != 0o666 {
		t.Errorf("seed file perm after EPERM: got %04o, want 0666", perm)
	}
}

// TestScratchPrepareNonEPERMChownErrorPropagates ensures a non-EPERM chown error
// causes Prepare to return that error (no silent fallback).
func TestScratchPrepareNonEPERMChownErrorPropagates(t *testing.T) {
	orig := osChown
	osChown = func(name string, uid, gid int) error {
		return &os.PathError{Op: "chown", Path: name, Err: syscall.ENOSYS}
	}
	defer func() { osChown = orig }()

	s := NewScratch(t.TempDir())
	_, err := s.Prepare(context.Background(), "spawn5", "data", "/no/such/seed", 1234)
	if err == nil {
		t.Fatal("expected error from non-EPERM chown failure, got nil")
	}
}

// TestNormalizeOwnershipPrivilegedPreservesMode: when chown into the agent uid succeeds
// (privileged/cloud node), restored entries are chowned and their modes are PRESERVED — a 0600
// file stays 0600 (owned by agent-root → writable), not loosened to world-writable.
func TestNormalizeOwnershipPrivilegedPreservesMode(t *testing.T) {
	dir := t.TempDir()
	// Simulate a journal restore: a 0600 file + a nested dir/file, all node-owned.
	if err := os.WriteFile(filepath.Join(dir, "data-pass"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var chowned []string
	orig := osChown
	osChown = func(name string, uid, gid int) error {
		if uid != 4242 || gid != 4242 {
			t.Errorf("chown %s to %d:%d, want 4242:4242", name, uid, gid)
		}
		chowned = append(chowned, filepath.Base(name))
		return nil // privileged: succeeds
	}
	defer func() { osChown = orig }()

	if err := NormalizeOwnership(dir, 4242); err != nil {
		t.Fatalf("NormalizeOwnership: %v", err)
	}
	// Every entry (root, data-pass, sub, sub/f) chowned to the agent uid.
	if len(chowned) != 4 {
		t.Fatalf("chowned %v, want 4 entries", chowned)
	}
	// Modes preserved (not loosened) on the privileged path.
	if m := statPerm(t, filepath.Join(dir, "data-pass")); m != 0o600 {
		t.Errorf("data-pass mode = %04o, want 0600 (preserved)", m)
	}
	if m := statPerm(t, filepath.Join(dir, "sub", "f")); m != 0o644 {
		t.Errorf("sub/f mode = %04o, want 0644 (preserved)", m)
	}
}

// TestNormalizeOwnershipEPERMFallbackWorldWritable: on a rootless node chown is denied, so the
// restored tree must be made world-writable instead (the agent can't be made owner). This is the
// dev-node case that produced the `nobody`/0600 unwritable bug.
func TestNormalizeOwnershipEPERMFallbackWorldWritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data-pass"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	orig := osChown
	osChown = func(string, int, int) error { return os.ErrPermission }
	defer func() { osChown = orig }()

	if err := NormalizeOwnership(dir, 100000); err != nil {
		t.Fatalf("NormalizeOwnership: %v", err)
	}
	if m := statPerm(t, filepath.Join(dir, "data-pass")); m&0o006 != 0o006 {
		t.Errorf("data-pass mode = %04o, want world rw (agent must be able to write)", m)
	}
	if m := statPerm(t, filepath.Join(dir, "sub")); m&0o007 != 0o007 {
		t.Errorf("sub dir mode = %04o, want world rwx (agent must traverse+create)", m)
	}
}

// TestNormalizeOwnershipDegradedNoChown: agentUID<0 (no-userns degraded lane) never chowns and
// makes the tree world-writable so the cap-dropped agent uid can write regardless of mapping.
func TestNormalizeOwnershipDegradedNoChown(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	chownCalled := false
	orig := osChown
	osChown = func(string, int, int) error { chownCalled = true; return nil }
	defer func() { osChown = orig }()

	if err := NormalizeOwnership(dir, -1); err != nil {
		t.Fatalf("NormalizeOwnership: %v", err)
	}
	if chownCalled {
		t.Error("degraded lane must not attempt chown")
	}
	if m := statPerm(t, filepath.Join(dir, "f")); m&0o006 != 0o006 {
		t.Errorf("f mode = %04o, want world rw", m)
	}
}

func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
