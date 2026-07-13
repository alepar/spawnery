package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCAStoreSaveLoadRoundTrips(t *testing.T) {
	s := caStore{dir: t.TempDir()}
	want := caPair{certPEM: []byte("cert-bytes"), keyPEM: []byte("key-bytes")}

	if err := s.Save("sp1", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Load("sp1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("Load: ok = false, want true")
	}
	if string(got.certPEM) != string(want.certPEM) || string(got.keyPEM) != string(want.keyPEM) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestCAStoreLoadAbsentIsNotError(t *testing.T) {
	s := caStore{dir: t.TempDir()}
	got, ok, err := s.Load("no-such-spawn")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok {
		t.Fatalf("Load: ok = true, want false (absent, not an error): %+v", got)
	}
}

func TestCAStorePermissions(t *testing.T) {
	dir := t.TempDir()
	s := caStore{dir: dir}
	if err := s.Save("sp1", caPair{certPEM: []byte("c"), keyPEM: []byte("k")}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	spawnDir := filepath.Join(dir, "sp1")
	info, err := os.Stat(spawnDir)
	if err != nil {
		t.Fatalf("stat spawn dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("spawn dir perm = %o, want 0700", perm)
	}

	keyInfo, err := os.Stat(filepath.Join(spawnDir, "ca.key"))
	if err != nil {
		t.Fatalf("stat ca.key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("ca.key perm = %o, want 0600", perm)
	}
}

func TestCAStorePartialStoreTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	s := caStore{dir: dir}
	// Simulate a torn write: only the cert made it to disk.
	spawnDir := filepath.Join(dir, "sp1")
	if err := os.MkdirAll(spawnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spawnDir, "ca.crt"), []byte("cert-only"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.Load("sp1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok {
		t.Fatalf("Load of a partial store: ok = true, want false (regenerate rather than serve a keyless CA): %+v", got)
	}
}

func TestCAStoreRemoveDeletesAndIsIdempotent(t *testing.T) {
	s := caStore{dir: t.TempDir()}
	if err := s.Save("sp1", caPair{certPEM: []byte("c"), keyPEM: []byte("k")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Remove("sp1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, err := s.Load("sp1"); err != nil || ok {
		t.Fatalf("Load after Remove: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	// Idempotent: a second Remove of an already-absent spawn is not an error.
	if err := s.Remove("sp1"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestCAStoreZeroValueIsMemoryOnlyNoOp(t *testing.T) {
	var s caStore // dir == ""
	if err := s.Save("sp1", caPair{certPEM: []byte("c"), keyPEM: []byte("k")}); err != nil {
		t.Fatalf("Save on zero-value store: %v", err)
	}
	if _, ok, err := s.Load("sp1"); err != nil || ok {
		t.Fatalf("Load on zero-value store: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if err := s.Remove("sp1"); err != nil {
		t.Fatalf("Remove on zero-value store: %v", err)
	}
}

func TestCAStoreRejectsUnsafeSpawnID(t *testing.T) {
	s := caStore{dir: t.TempDir()}
	for _, id := range []string{"../escape", "a/b", ".."} {
		if err := s.Save(id, caPair{certPEM: []byte("c"), keyPEM: []byte("k")}); err == nil {
			t.Fatalf("Save(%q): want error for unsafe spawn id", id)
		}
		if _, _, err := s.Load(id); err == nil {
			t.Fatalf("Load(%q): want error for unsafe spawn id", id)
		}
		if err := s.Remove(id); err == nil {
			t.Fatalf("Remove(%q): want error for unsafe spawn id", id)
		}
	}
}
