package spawnlet

import (
	"context"
	"strings"
	"testing"

	"spawnery/internal/manifest"
	"spawnery/internal/storage"
)

type fakeRestoreAware struct {
	pending   bool
	pendingOK bool
}

func (f *fakeRestoreAware) Prepare(context.Context, string, string, string, int) (string, error) {
	return "", nil
}
func (f *fakeRestoreAware) Finalize(context.Context, string) error { return nil }
func (f *fakeRestoreAware) SetRestorePending(p bool)               { f.pending, f.pendingOK = p, true }

func TestApplyRestoreHintSetsPendingOnRestoreAware(t *testing.T) {
	f := &fakeRestoreAware{}
	applyRestoreHint(f, true)
	if !f.pendingOK || !f.pending {
		t.Fatalf("SetRestorePending(true) not propagated: ok=%v pending=%v", f.pendingOK, f.pending)
	}
	applyRestoreHint(f, false)
	if f.pending {
		t.Fatalf("SetRestorePending(false) not propagated")
	}
}

func TestApplyRestoreHintNoopOnPlainBackend(t *testing.T) {
	// A backend without SetRestorePending must not panic.
	applyRestoreHint(storage.NewScratch(t.TempDir()), true)
}

func TestMountBindingsByNameRejectsEmptyBindingName(t *testing.T) {
	t.Parallel()

	_, err := mountBindingsByName([]manifest.Mount{{Name: ""}}, []MountBinding{{Name: "", BackendURI: "scratch:"}})
	if err == nil {
		t.Fatal("mountBindingsByName should reject empty binding names")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "empty") {
		t.Fatalf("mountBindingsByName error = %q, want empty-name detail", err)
	}
}

func TestMountBindingsByNameRejectsDuplicateBinding(t *testing.T) {
	t.Parallel()

	_, err := mountBindingsByName(
		[]manifest.Mount{{Name: "main"}},
		[]MountBinding{
			{Name: "main", BackendURI: "scratch:"},
			{Name: "main", BackendURI: "github:o/r"},
		},
	)
	if err == nil {
		t.Fatal("mountBindingsByName should reject duplicate bindings for the same mount name")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Errorf("error = %q, want 'duplicate' in message", err)
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("error = %q, want mount name 'main' in message", err)
	}
}

func TestMountBindingsByNameRejectsBindingForMountNotInManifest(t *testing.T) {
	t.Parallel()

	_, err := mountBindingsByName(
		[]manifest.Mount{{Name: "main"}},
		[]MountBinding{{Name: "cache", BackendURI: "scratch:"}},
	)
	if err == nil {
		t.Fatal("mountBindingsByName should reject bindings for mounts not in the manifest")
	}
	if !strings.Contains(err.Error(), "cache") {
		t.Errorf("error = %q, want binding name 'cache' in message", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "manifest") {
		t.Errorf("error = %q, want 'manifest' in message", err)
	}
}

func TestMountBindingsByNameMapsOnlyBoundMounts(t *testing.T) {
	t.Parallel()

	result, err := mountBindingsByName(
		[]manifest.Mount{{Name: "main"}, {Name: "cache"}},
		[]MountBinding{{Name: "main", BackendURI: "github:o/r"}},
	)
	if err != nil {
		t.Fatalf("mountBindingsByName: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("result has %d entries, want 1 (only the bound mount)", len(result))
	}
	if b, ok := result["main"]; !ok || b.BackendURI != "github:o/r" {
		t.Errorf("result[main] = %+v, want BackendURI 'github:o/r'", b)
	}
	if _, ok := result["cache"]; ok {
		t.Error("unbound 'cache' mount should not appear in result map; default is applied later")
	}
}
