package token

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
)

func TestSignerRevocationStorePersistsAndReloads(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	store, err := OpenSignerRevocationStore(path, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	if store.Generation() != 0 {
		t.Fatalf("missing store generation = %d", store.Generation())
	}
	statement := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	if err := store.Apply(statement); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode = %o", dirInfo.Mode().Perm())
	}

	reopened, err := OpenSignerRevocationStore(path, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Generation() != 1 {
		t.Fatalf("reopened generation = %d", reopened.Generation())
	}
	older := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Add(-time.Second).Unix()})
	if err := reopened.Apply(older); !errors.Is(err, ErrRevocationEquivocation) {
		t.Fatalf("reopened store accepted conflicting generation: %v", err)
	}
}

func TestSignerRevocationStoreFailsClosedOnCorruptionAndPermissions(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	for _, tc := range []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{name: "truncated", content: []byte(`{"version":1`), mode: 0o600},
		{name: "unknown fields", content: []byte(`{"version":1,"envelope":"x","sha256":"x","extra":true}`), mode: 0o600},
		{name: "permissive", content: []byte(`{}`), mode: 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, tc.content, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenSignerRevocationStore(path, pki.root, "prod", now); err == nil {
				t.Fatal("invalid persisted state accepted")
			}
		})
	}
}

func TestSignerRevocationStoreRejectsNonPrivateExistingDirectory(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSignerRevocationStore(filepath.Join(dir, "state.json"), pki.root, "prod", now); err == nil {
		t.Fatal("non-private existing directory accepted")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("opening store mutated existing directory mode to %o", info.Mode().Perm())
	}
}

func TestSignerRevocationStoreRevalidatesOnOpen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	store, err := OpenSignerRevocationStore(path, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	statement := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	if err := store.Apply(statement); err != nil {
		t.Fatal(err)
	}
	other := newCertTestPKI(t, nil)
	if _, err := OpenSignerRevocationStore(path, other.root, "prod", now); err == nil {
		t.Fatal("persisted envelope trusted under wrong root")
	}
	if _, err := OpenSignerRevocationStore(path, pki.root, "staging", now); err == nil {
		t.Fatal("persisted envelope trusted in wrong environment")
	}
}

func TestSignerRevocationStoreAtomicFailurePreservesOldGeneration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	path := filepath.Join(t.TempDir(), "revocations", "state.json")
	store, _ := OpenSignerRevocationStore(path, pki.root, "prod", now)
	first := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	second := mustParseRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 2, IssuedAt: now.Unix()})
	if err := store.Apply(first); err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return errors.New("injected pre-rename failure") }
	if err := store.Apply(second); err == nil {
		t.Fatal("injected persistence failure ignored")
	}
	if store.Generation() != 1 {
		t.Fatalf("memory advanced after failed persistence: %d", store.Generation())
	}
	reopened, err := OpenSignerRevocationStore(path, pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Generation() != 1 {
		t.Fatalf("disk advanced after failed persistence: %d", reopened.Generation())
	}
}

func TestSignerRevocationStoreLoadAndApply(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pki := newCertTestPKI(t, nil)
	store, err := OpenSignerRevocationStore(filepath.Join(t.TempDir(), "revocations", "state.json"), pki.root, "prod", now)
	if err != nil {
		t.Fatal(err)
	}
	configuredPath := filepath.Join(t.TempDir(), "configured.statement")
	if err := store.LoadAndApply(configuredPath, now); err != nil {
		t.Fatalf("absent initial statement: %v", err)
	}
	wire := mustSignRevocation(t, pki, &authv1.SignerRevocationStatement{Environment: "prod", Generation: 1, IssuedAt: now.Unix()})
	if err := os.WriteFile(configuredPath, []byte(wire+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAndApply(configuredPath, now); err != nil {
		t.Fatal(err)
	}
	if store.Generation() != 1 {
		t.Fatalf("loaded generation = %d", store.Generation())
	}
	if err := os.Remove(configuredPath); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAndApply(configuredPath, now); err == nil {
		t.Fatal("absent statement allowed after state was established")
	}
	if store.Generation() != 1 {
		t.Fatal("failed reload changed state")
	}
	if err := os.WriteFile(configuredPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAndApply(configuredPath, now); err == nil {
		t.Fatal("corrupt configured statement accepted")
	}
	if store.Generation() != 1 {
		t.Fatal("corrupt reload changed state")
	}
}
