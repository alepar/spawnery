package spawnlet

import (
	"os"
	"path/filepath"
	"testing"
)

// --- GitEnv tests ---

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

func TestSecretInjectorWrites0600(t *testing.T) {
	root := t.TempDir()
	inj := SecretInjector{Root: root}
	path, err := inj.Write("sp1", "gh/hosts.yml", []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "sp1", "gh", "hosts.yml"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "token" {
		t.Fatalf("read = %q err=%v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	// Re-delivery overwrites (O_TRUNC), and stays 0600.
	if _, err := inj.Write("sp1", "gh/hosts.yml", []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("overwrite = %q, want new", got)
	}
}

// An absolute target is treated as relative to the per-spawn mount root (leading slash stripped).
func TestSecretInjectorAbsoluteTargetIsMountRelative(t *testing.T) {
	root := t.TempDir()
	inj := SecretInjector{Root: root}
	path, err := inj.Write("sp1", "/etc/secret", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "sp1", "etc", "secret"); path != want {
		t.Fatalf("path = %s, want %s (absolute target must stay within the per-spawn dir)", path, want)
	}
}

// A traversal target must be rejected — a malicious delivery cannot escape the secrets dir.
func TestSecretInjectorRejectsTraversal(t *testing.T) {
	inj := SecretInjector{Root: t.TempDir()}
	for _, bad := range []string{"../escape", "a/../../escape", "..", ""} {
		if _, err := inj.Write("sp1", bad, []byte("x")); err == nil {
			t.Fatalf("Write(%q) must be rejected", bad)
		}
	}
}

func TestSecretInjectorRemove(t *testing.T) {
	root := t.TempDir()
	inj := SecretInjector{Root: root}
	if _, err := inj.Write("sp1", "a/b", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := inj.Remove("sp1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inj.DirFor("sp1")); !os.IsNotExist(err) {
		t.Fatalf("secrets dir must be gone after Remove, err=%v", err)
	}
	// Removing a non-existent dir is not an error.
	if err := inj.Remove("nope"); err != nil {
		t.Fatalf("Remove of missing dir: %v", err)
	}
}
