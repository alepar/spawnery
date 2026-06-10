package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNodeKey(t *testing.T, dir string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, "node.key")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNodeLocalCustodySealUnsealRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyfile := writeNodeKey(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	sealDir := filepath.Join(dir, "seals")

	c, err := NewNodeLocalCustody(keyfile, sealDir)
	if err != nil {
		t.Fatalf("custody: %v", err)
	}
	pw1, err := c.PasswordFor("spawn-A")
	if err != nil {
		t.Fatalf("password: %v", err)
	}
	if pw1 == "" {
		t.Fatal("empty password")
	}
	// Same spawn ⇒ same password (cached).
	if pw1b, _ := c.PasswordFor("spawn-A"); pw1b != pw1 {
		t.Fatal("password not stable for same spawn")
	}
	// Different spawn ⇒ different password.
	if pw2, _ := c.PasswordFor("spawn-B"); pw2 == pw1 {
		t.Fatal("distinct spawns must get distinct passwords")
	}

	// A fresh custody instance (same keyfile+sealdir) must unseal the same
	// password — same-node resume / crash recovery.
	c2, err := NewNodeLocalCustody(keyfile, sealDir)
	if err != nil {
		t.Fatal(err)
	}
	pwReopen, err := c2.PasswordFor("spawn-A")
	if err != nil {
		t.Fatalf("reopen password: %v", err)
	}
	if pwReopen != pw1 {
		t.Fatalf("reopened password %q != original %q", pwReopen, pw1)
	}
}

func TestNodeLocalCustodyWrongNodeKeyCannotUnseal(t *testing.T) {
	dir := t.TempDir()
	keyfile := writeNodeKey(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	sealDir := filepath.Join(dir, "seals")

	c, err := NewNodeLocalCustody(keyfile, sealDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PasswordFor("spawn-A"); err != nil {
		t.Fatal(err)
	}

	// A different node key over the same seal dir must fail to unseal (the
	// AEAD tag rejects it) — a different node cannot read a node-local password.
	otherKeyfile := writeNodeKey(t, t.TempDir(), []byte("ffffffffffffffffffffffffffffffff"))
	cOther, err := NewNodeLocalCustody(otherKeyfile, sealDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cOther.PasswordFor("spawn-A"); err == nil {
		t.Fatal("expected unseal failure with wrong node key")
	}
}

func TestNodeLocalCustodyForget(t *testing.T) {
	dir := t.TempDir()
	keyfile := writeNodeKey(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	sealDir := filepath.Join(dir, "seals")
	c, err := NewNodeLocalCustody(keyfile, sealDir)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := c.PasswordFor("spawn-A")

	if err := c.Forget("spawn-A"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	// Forget on a missing spawn is not an error.
	if err := c.Forget("spawn-missing"); err != nil {
		t.Fatalf("forget missing: %v", err)
	}
	// After Forget, a new PasswordFor mints a FRESH password (the old seal is gone).
	if pw2, _ := c.PasswordFor("spawn-A"); pw2 == pw {
		t.Fatal("expected a fresh password after Forget")
	}
}

func TestNodeLocalCustodyRejectsShortKey(t *testing.T) {
	dir := t.TempDir()
	keyfile := writeNodeKey(t, dir, []byte("too-short"))
	if _, err := NewNodeLocalCustody(keyfile, filepath.Join(dir, "seals")); err == nil {
		t.Fatal("expected error for short node key")
	}
}

func TestGenerateNodeKeyfileIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "node.key")
	if err := GenerateNodeKeyfile(p); err != nil {
		t.Fatalf("generate: %v", err)
	}
	b1, _ := os.ReadFile(p)
	if len(b1) != 32 {
		t.Fatalf("keyfile should be 32 bytes, got %d", len(b1))
	}
	// Second call leaves the existing key untouched.
	if err := GenerateNodeKeyfile(p); err != nil {
		t.Fatalf("generate idempotent: %v", err)
	}
	b2, _ := os.ReadFile(p)
	if string(b1) != string(b2) {
		t.Fatal("GenerateNodeKeyfile must not overwrite an existing key")
	}
}
