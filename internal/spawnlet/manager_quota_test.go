package spawnlet

// manager_quota_test.go: tests for the delta quota watchdog (spec §7, task .12).
//
// Test matrix:
//   Q1: Hard threshold stops the spawn (store empty after stop; delta NOT released by Stop).
//   Q2: Soft threshold suspends the spawn (delta captured, spawn removed from store).
//   Q3: Under-threshold: no-op (spawn still live, no capture).
//   Q4: Backend without DeltaSize → CheckQuotas is dormant (spawn still live).

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

// noSizeFakeBackend is a PodBackend that does NOT implement deltaSizer (no DeltaSize method).
// It delegates all PodBackend methods to an inner *fakePodBackend via explicit forwarding
// (NOT embedding) so that method promotion cannot accidentally surface DeltaSize on
// *noSizeFakeBackend.  The type-assertion in CheckQuotas must fail, triggering the
// dormant-quota path.
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

// Compile-time assertion: *noSizeFakeBackend must satisfy PodBackend but NOT deltaSizer.
var _ runtime.PodBackend = (*noSizeFakeBackend)(nil)

// newNoSizeFakeBackend returns a PodBackend without DeltaSize for dormant-quota tests.
func newNoSizeFakeBackend() *noSizeFakeBackend {
	return &noSizeFakeBackend{inner: &fakePodBackend{}}
}

// Q1: Hard threshold stops the spawn; Stop does NOT release the delta image.
func TestQuotaHardStopsSpawn(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 200} // reports 200 MiB
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaHardMB: 100, // threshold = 100 MiB; spawn at 200 → hard stop
	}))

	sp, err := m.Create(ctx, "sp-hard", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	m.CheckQuotas(ctx)

	// Spawn must be removed from store (Stop was called).
	if _, live := m.Store().Get(sp.ID); live {
		t.Fatal("hard quota: spawn should have been stopped and removed from store")
	}
	// Stop does NOT release the delta image (only Delete does).
	if fb.releasedSpawn != "" {
		t.Fatalf("hard quota: Stop must NOT release delta; releasedSpawn=%q", fb.releasedSpawn)
	}
}

// Q2: Soft threshold suspends the spawn (delta is captured, spawn removed from store).
func TestQuotaSoftSuspendsSpawn(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 60} // reports 60 MiB
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 50, // threshold = 50 MiB; spawn at 60 → soft suspend
		DeltaQuotaHardMB: 0,  // hard disabled
	}))

	sp, err := m.Create(ctx, "sp-soft", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	m.CheckQuotas(ctx)

	// Spawn must be removed from store (Suspend was called).
	if _, live := m.Store().Get(sp.ID); live {
		t.Fatal("soft quota: spawn should have been suspended and removed from store")
	}
	// Suspend captures the delta (DeltaCapture=true).
	if fb.capturedRef == "" {
		t.Fatal("soft quota: Suspend should have captured a delta")
	}
	// Suspend does NOT release the delta.
	if fb.releasedSpawn != "" {
		t.Fatalf("soft quota: Suspend must NOT release delta; releasedSpawn=%q", fb.releasedSpawn)
	}
}

// Q3: Under-threshold → no-op: spawn still live, no capture triggered.
func TestQuotaUnderThresholdNoOp(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 10} // reports 10 MiB
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 50,
		DeltaQuotaHardMB: 100,
	}))

	sp, err := m.Create(ctx, "sp-under", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	m.CheckQuotas(ctx)

	// Spawn must still be live.
	if _, live := m.Store().Get(sp.ID); !live {
		t.Fatal("under-threshold: spawn should still be live")
	}
	if fb.capturedRef != "" {
		t.Fatalf("under-threshold: no capture expected; capturedRef=%q", fb.capturedRef)
	}
}

// Q4: Backend without DeltaSize → CheckQuotas is dormant (spawn still live).
func TestQuotaDormantWhenNoSizeSource(t *testing.T) {
	ctx := context.Background()
	ns := newNoSizeFakeBackend()
	m := noScrub(NewManagerWithBackend(ns, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 1,
		DeltaQuotaHardMB: 1,
	}))

	sp, err := m.Create(ctx, "sp-nodelta", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First call should log "dormant" once and not affect the spawn.
	m.CheckQuotas(ctx)
	m.CheckQuotas(ctx) // second call: no second log (once guard)

	if _, live := m.Store().Get(sp.ID); !live {
		t.Fatal("no-size-source: spawn should still be live (quota dormant)")
	}
}
