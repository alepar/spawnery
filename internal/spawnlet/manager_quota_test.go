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
)

// noSizeFakeBackend is a minimal PodBackend that does NOT implement deltaSizer.
// Used to verify that CheckQuotas degrades gracefully when the backend lacks DeltaSize.
type noSizeFakeBackend struct {
	fakePodBackend
}

// Note: noSizeFakeBackend intentionally does NOT define DeltaSize so the deltaSizer
// interface assertion in CheckQuotas fails, triggering the dormant-quota path.

// Q1: Hard threshold stops the spawn; Stop does NOT release the delta image.
func TestQuotaHardStopsSpawn(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{deltaSizeMB: 200} // reports 200 MiB
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaHardMB: 100, // threshold = 100 MiB; spawn at 200 → hard stop
	})

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
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 50, // threshold = 50 MiB; spawn at 60 → soft suspend
		DeltaQuotaHardMB: 0,  // hard disabled
	})

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
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 50,
		DeltaQuotaHardMB: 100,
	})

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
	ns := &noSizeFakeBackend{}
	m := NewManagerWithBackend(ns, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaQuotaSoftMB: 1,
		DeltaQuotaHardMB: 1,
	})

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
