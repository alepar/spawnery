package spawnlet

import (
	"os"
	"path/filepath"
	"testing"
)

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
