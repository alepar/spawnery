package spawnlet

// manager_reconcile_capture_test.go: tests for capture-before-reap in ReapPod (spec §4), fed by
// UntrackedPods (the discovery half ReapOrphans used to do in one loop — see adopt.go).
//
// Test matrix:
//   R1: DeltaCapture=true → CaptureDelta happens BEFORE pod.Stop for a reaped agent.
//   R2: DeltaCapture=false → no CaptureDelta on reap.
//   R3: Foreign-NodeID pod is skipped entirely (not even reported by UntrackedPods).
//   R4: In-store spawn is not reported by UntrackedPods (not captured, not stopped).
//
// R1/R2 reap a REAL fakepod pod (m.Create + m.store.Delete): fakepod's Stop on an unknown handle is
// a silent no-op that records nothing, unlike the old fake, so the reaped pod must actually exist on
// the backend — a node restart, not a fabricated handle. R3/R4 assert nothing happens, so the scripted
// ListManaged override (a pod the backend never heard of) keeps their intent sharp.

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

// opsIndex returns the first index of op in ops, or -1 if not found.
func opsIndex(ops []string, op string) int {
	for i, o := range ops {
		if o == op {
			return i
		}
	}
	return -1
}

// R1: ReapPod with DeltaCapture=true calls CaptureDelta before pod.Stop.
func TestReapPodCaptureBefore_Stop(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	}))
	// The pod is really running on the backend, but the store has forgotten it — a node restart.
	// ListManaged derives it from the fake's live pods, labelled with NodeID=node-9.
	if _, err := m.Create(ctx, "orphan-1", writeApp(t), "model", "", "", 3); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.store.Delete("orphan-1")

	pods, err := m.UntrackedPods(ctx)
	if err != nil {
		t.Fatalf("UntrackedPods: %v", err)
	}
	if len(pods) != 1 || pods[0].SpawnID != "orphan-1" {
		t.Fatalf("UntrackedPods = %+v, want orphan-1", pods)
	}
	if err := m.ReapPod(ctx, pods[0]); err != nil {
		t.Fatalf("ReapPod: %v", err)
	}

	if lastOf(fb.CapturedRefs()) == "" {
		t.Fatal("CaptureDelta was not called for reaped pod with DeltaCapture=true")
	}
	ops := lifecycleOps(fb)
	captureIdx := opsIndex(ops, "capture:orphan-1")
	stopIdx := opsIndex(ops, "stop:orphan-1")
	if captureIdx < 0 {
		t.Fatalf("ops missing capture:orphan-1; ops=%v", ops)
	}
	if stopIdx < 0 {
		t.Fatalf("ops missing stop:orphan-1; ops=%v", ops)
	}
	if captureIdx >= stopIdx {
		t.Fatalf("capture must happen BEFORE stop; ops=%v", ops)
	}
}

// R2: DeltaCapture=false → no CaptureDelta on reap.
func TestReapPodNoCapture_WhenDisabled(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: false,
	}))
	if _, err := m.Create(ctx, "orphan-2", writeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.store.Delete("orphan-2")

	pods, err := m.UntrackedPods(ctx)
	if err != nil {
		t.Fatalf("UntrackedPods: %v", err)
	}
	if len(pods) != 1 || pods[0].SpawnID != "orphan-2" {
		t.Fatalf("UntrackedPods = %+v, want orphan-2", pods)
	}
	if err := m.ReapPod(ctx, pods[0]); err != nil {
		t.Fatalf("ReapPod: %v", err)
	}

	if lastOf(fb.CapturedRefs()) != "" {
		t.Fatalf("CaptureDelta must NOT be called when DeltaCapture=false; got capturedRef=%q", lastOf(fb.CapturedRefs()))
	}
	if opsIndex(lifecycleOps(fb), "stop:orphan-2") < 0 {
		t.Fatal("pod.Stop should still be called even when DeltaCapture=false")
	}
}

// R3: Foreign-NodeID pod is not even reported by UntrackedPods (neither captured nor stopped).
func TestUntrackedPodsForeignNodeSkipped(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t, fakepod.WithListManaged([]runtime.ManagedPod{
		{SpawnID: "foreign-spawn", AgentID: "ag-foreign", NodeID: "other-node", Generation: 1},
	}))
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	})

	pods, err := m.UntrackedPods(ctx)
	if err != nil {
		t.Fatalf("UntrackedPods: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("must NOT report foreign node's pod: %+v", pods)
	}
	if lastOf(fb.CapturedRefs()) != "" {
		t.Fatalf("must NOT capture foreign node's pod; capturedRef=%q", lastOf(fb.CapturedRefs()))
	}
	if fb.LastStopHandle() != nil {
		t.Fatalf("must NOT stop foreign node's pod; stopped=%v", fb.LastStopHandle())
	}
}

// R4: A spawn that is still in the store is not reported by UntrackedPods.
func TestUntrackedPodsSkipsLiveSpawn(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t, fakepod.WithListManaged([]runtime.ManagedPod{
		{SpawnID: "live-spawn", AgentID: "ag-live", NodeID: "node-9", Generation: 1},
	}))
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	})
	// Insert into store to simulate a live spawn.
	m.store.Put(&Spawn{ID: "live-spawn", AgentID: "ag-live"})

	pods, err := m.UntrackedPods(ctx)
	if err != nil {
		t.Fatalf("UntrackedPods: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("must NOT report a still-live spawn: %+v", pods)
	}
	if lastOf(fb.CapturedRefs()) != "" {
		t.Fatalf("must NOT capture a still-live spawn; capturedRef=%q", lastOf(fb.CapturedRefs()))
	}
	if len(lifecycleOps(fb)) != 0 {
		t.Fatalf("no ops expected for live spawn; ops=%v", lifecycleOps(fb))
	}
}
