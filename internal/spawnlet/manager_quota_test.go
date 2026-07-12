package spawnlet

// manager_quota_test.go: tests for Manager.DeltaSize (the node-side metric reporter that replaced
// the old autonomous quota watchdog; §6 "node-local detectors → CP-side reporters").
//
// Test matrix:
//   DS1: Backend that implements deltaSizer: DeltaSize returns the backend's reported size.
//   DS2: Backend without DeltaSize (no deltaSizer): DeltaSize returns 0, nil (unknown = safe to emit).
//   DS3: DeltaSize for a live spawn with a sized backend: delegates to the backend.

import (
	"context"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// DS1: Backend that implements deltaSizer: DeltaSize returns the backend's reported size.
func TestDeltaSizeReturnsBackendSize(t *testing.T) {
	ctx := context.Background()
	wantBytes := int64(42) << 20 // 42 MiB
	fb := fakeBackend(t, fakepod.WithDeltaSizeBytes(wantBytes))
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp1", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sz, serr := m.DeltaSize(ctx, sp.ID)
	if serr != nil {
		t.Fatalf("DeltaSize: %v", serr)
	}
	if sz != wantBytes {
		t.Fatalf("DeltaSize=%d want %d (42 MiB)", sz, wantBytes)
	}
}

// DS2: Backend without DeltaSize → DeltaSize returns 0, nil (unknown = safe to emit).
// fakepod.WithoutDeltaSize hides the method by embedding the INTERFACE, so Manager.DeltaSize's
// deltaSizer type assertion must fail.
func TestDeltaSizeReturnsZeroWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	ns := fakepod.WithoutDeltaSize(fakeBackend(t))
	m := noScrub(NewManagerWithBackend(ns, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp1", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sz, serr := m.DeltaSize(ctx, sp.ID)
	if serr != nil {
		t.Fatalf("DeltaSize (no-size backend) must return nil error, got: %v", serr)
	}
	if sz != 0 {
		t.Fatalf("DeltaSize (no-size backend) must return 0, got %d", sz)
	}
}

// DS3: DeltaSize for a live spawn with a sized backend: delegates to the backend.
func TestDeltaSizeForLiveSpawn(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t, fakepod.WithDeltaSizeBytes(5<<20))
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp1", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sz, serr := m.DeltaSize(ctx, sp.ID)
	if serr != nil {
		t.Fatalf("DeltaSize: %v", serr)
	}
	if sz == 0 {
		t.Fatal("DeltaSize with a sized backend must return non-zero for a live spawn")
	}
}
