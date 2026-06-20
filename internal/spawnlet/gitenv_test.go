package spawnlet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitEnvPrepareChownsToAgentUID(t *testing.T) {
	root := t.TempDir()
	var gotUID int
	chownCalled := false
	orig := gitEnvChown
	gitEnvChown = func(name string, uid, gid int) error {
		chownCalled = true
		gotUID = uid
		return nil
	}
	defer func() { gitEnvChown = orig }()

	g := GitEnv{Root: root}
	dir, err := g.Prepare("sp1", 100000)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	want := filepath.Join(root, "sp1")
	if dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir does not exist: %v", err)
	}
	if !chownCalled {
		t.Fatal("chown seam was not called")
	}
	if gotUID != 100000 {
		t.Fatalf("chown uid = %d, want 100000", gotUID)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}
}

func TestGitEnvPrepareDegradedWorldWritableOnEPERM(t *testing.T) {
	root := t.TempDir()
	orig := gitEnvChown
	gitEnvChown = func(name string, uid, gid int) error {
		return os.ErrPermission
	}
	defer func() { gitEnvChown = orig }()

	g := GitEnv{Root: root}
	dir, err := g.Prepare("sp1", 100000)
	if err != nil {
		t.Fatalf("Prepare must not fail on EPERM: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o777 {
		t.Fatalf("degraded mode = %v, want 0777", fi.Mode().Perm())
	}
}

func TestGitEnvPrepareDegradedWhenAgentUIDNegative(t *testing.T) {
	root := t.TempDir()
	chownCalled := false
	orig := gitEnvChown
	gitEnvChown = func(name string, uid, gid int) error {
		chownCalled = true
		return nil
	}
	defer func() { gitEnvChown = orig }()

	g := GitEnv{Root: root}
	dir, err := g.Prepare("sp1", -1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if chownCalled {
		t.Fatal("chown seam must NOT be called when agentUID < 0")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o777 {
		t.Fatalf("degraded mode = %v, want 0777", fi.Mode().Perm())
	}
}

func TestGitEnvRemove(t *testing.T) {
	root := t.TempDir()
	orig := gitEnvChown
	gitEnvChown = func(name string, uid, gid int) error { return nil }
	defer func() { gitEnvChown = orig }()

	g := GitEnv{Root: root}
	if _, err := g.Prepare("sp1", 0); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := g.Remove("sp1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(g.DirFor("sp1")); !os.IsNotExist(err) {
		t.Fatalf("git-env dir must be gone after Remove, err=%v", err)
	}
	// Removing a non-existent dir is not an error.
	if err := g.Remove("nope"); err != nil {
		t.Fatalf("Remove of missing dir: %v", err)
	}
}
