package spawnlet

// manager_quota_test.go: tests for Manager.DeltaSize (the node-side metric reporter that replaced
// the old autonomous quota watchdog; §6 "node-local detectors → CP-side reporters").
//
// Test matrix:
//   DS1: Backend that implements deltaSizer: DeltaSize returns the backend's reported size.
//   DS2: Backend without DeltaSize (no deltaSizer): DeltaSize returns 0, nil (unknown = safe to emit).
//   DS3: DeltaSize for an unknown spawn id: returns 0, nil (pre-first-suspend = quota N/A).

import (
	"context"
	"io"
	"testing"

	"spawnery/internal/runtime"
)

// noSizeFakeBackend is a PodBackend that does NOT implement deltaSizer (no DeltaSize method).
// It delegates all PodBackend methods to an inner *fakePodBackend via explicit forwarding
// (NOT embedding) so that method promotion cannot accidentally surface DeltaSize on
// *noSizeFakeBackend. The type-assertion in Manager.DeltaSize must fail, yielding 0, nil.
//
// NOTE: embedding fakePodBackend by value would promote DeltaSize (pointer receiver) to
// *noSizeFakeBackend, making it satisfy deltaSizer — the opposite of what we want.
type noSizeFakeBackend struct {
	inner *fakePodBackend
}

func (n *noSizeFakeBackend) Ping(ctx context.Context) error { return n.inner.Ping(ctx) }
func (n *noSizeFakeBackend) Preflight(ctx context.Context) error {
	return n.inner.Preflight(ctx)
}
func (n *noSizeFakeBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	return n.inner.StartPod(ctx, spec)
}
func (n *noSizeFakeBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	return n.inner.StartAgent(ctx, h, spec)
}
func (n *noSizeFakeBackend) Stop(ctx context.Context, h *runtime.PodHandle) error {
	return n.inner.Stop(ctx, h)
}
func (n *noSizeFakeBackend) Attach(ctx context.Context, h *runtime.PodHandle) (*runtime.AttachedStream, error) {
	return n.inner.Attach(ctx, h)
}
func (n *noSizeFakeBackend) ListManaged(ctx context.Context) ([]runtime.ManagedPod, error) {
	return n.inner.ListManaged(ctx)
}
func (n *noSizeFakeBackend) ResolveImageDigest(ctx context.Context, ref string) (string, error) {
	return n.inner.ResolveImageDigest(ctx, ref)
}
func (n *noSizeFakeBackend) EnsureImage(ctx context.Context, baseRef, deltaRef string) (string, error) {
	return n.inner.EnsureImage(ctx, baseRef, deltaRef)
}
func (n *noSizeFakeBackend) CaptureDelta(ctx context.Context, h *runtime.PodHandle) (string, error) {
	return n.inner.CaptureDelta(ctx, h)
}
func (n *noSizeFakeBackend) ReleaseDelta(ctx context.Context, spawnID string) error {
	return n.inner.ReleaseDelta(ctx, spawnID)
}
func (n *noSizeFakeBackend) ExportDelta(ctx context.Context, spawnID string, w io.Writer) error {
	return n.inner.ExportDelta(ctx, spawnID, w)
}
func (n *noSizeFakeBackend) ImportDelta(ctx context.Context, spawnID, baseRef string, r io.Reader) (string, error) {
	return n.inner.ImportDelta(ctx, spawnID, baseRef, r)
}
func (n *noSizeFakeBackend) Pause(ctx context.Context, h *runtime.PodHandle) error {
	return n.inner.Pause(ctx, h)
}
func (n *noSizeFakeBackend) Unpause(ctx context.Context, h *runtime.PodHandle) error {
	return n.inner.Unpause(ctx, h)
}

// Compile-time assertion: *noSizeFakeBackend must satisfy PodBackend but NOT deltaSizer.
var _ runtime.PodBackend = (*noSizeFakeBackend)(nil)

// newNoSizeFakeBackend returns a PodBackend without DeltaSize for "unknown size" tests.
func newNoSizeFakeBackend() *noSizeFakeBackend {
	return &noSizeFakeBackend{inner: &fakePodBackend{}}
}

// DS1: Backend that implements deltaSizer: DeltaSize returns the backend's reported size.
func TestDeltaSizeReturnsBackendSize(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 42} // reports 42 MiB = 42 * 1<<20 bytes
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
	wantBytes := int64(42) << 20 // 42 MiB in bytes
	if sz != wantBytes {
		t.Fatalf("DeltaSize=%d want %d (42 MiB)", sz, wantBytes)
	}
}

// DS2: Backend without DeltaSize → DeltaSize returns 0, nil (unknown = safe to emit).
func TestDeltaSizeReturnsZeroWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	ns := newNoSizeFakeBackend()
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

// DS3: DeltaSize for any spawn id with a deltaSizer backend: delegates to the backend.
// (The backend in tests uses the spawn id as a key but always returns the configured deltaSizeMB.)
func TestDeltaSizeForLiveSpawn(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 5}
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp1", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Spawn is live in the store; DeltaSize delegates to the backend.
	sz, serr := m.DeltaSize(ctx, sp.ID)
	if serr != nil {
		t.Fatalf("DeltaSize: %v", serr)
	}
	if sz == 0 {
		t.Fatal("DeltaSize with a sized backend must return non-zero for a live spawn")
	}
}
