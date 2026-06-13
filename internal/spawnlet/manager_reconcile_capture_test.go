package spawnlet

// manager_reconcile_capture_test.go: tests for capture-before-reap in ReapOrphans (spec §4).
//
// Test matrix:
//   R1: DeltaCapture=true → CaptureDelta happens BEFORE pod.Stop for orphaned agent.
//   R2: DeltaCapture=false → no CaptureDelta on orphan reap.
//   R3: Foreign-NodeID orphan is skipped entirely (neither captured nor stopped).
//   R4: In-store spawn is not treated as orphan (not captured, not stopped).

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
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

// R1: ReapOrphans with DeltaCapture=true calls CaptureDelta before pod.Stop.
func TestReapOrphansCaptureBefore_Stop(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{
		listManaged: []runtime.ManagedPod{
			{SpawnID: "orphan-1", AgentID: "ag-orphan", NodeID: "node-9", Generation: 3},
		},
	}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	})
	// Note: orphan-1 is NOT in the store (simulates node restart).

	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	// CaptureDelta must have been called.
	if fb.capturedRef == "" {
		t.Fatal("CaptureDelta was not called for orphaned pod with DeltaCapture=true")
	}
	// CaptureDelta must have happened BEFORE pod.Stop.
	captureIdx := opsIndex(fb.ops, "capture:orphan-1")
	stopIdx := opsIndex(fb.ops, "stop")
	if captureIdx < 0 {
		t.Fatalf("ops missing capture:orphan-1; ops=%v", fb.ops)
	}
	if stopIdx < 0 {
		t.Fatalf("ops missing stop; ops=%v", fb.ops)
	}
	if captureIdx >= stopIdx {
		t.Fatalf("capture must happen BEFORE stop; ops=%v", fb.ops)
	}
}

// R2: DeltaCapture=false → no CaptureDelta on orphan reap.
func TestReapOrphansNoCapture_WhenDisabled(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{
		listManaged: []runtime.ManagedPod{
			{SpawnID: "orphan-2", AgentID: "ag-orphan", NodeID: "node-9", Generation: 1},
		},
	}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: false,
	})

	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if fb.capturedRef != "" {
		t.Fatalf("CaptureDelta must NOT be called when DeltaCapture=false; got capturedRef=%q", fb.capturedRef)
	}
	if opsIndex(fb.ops, "stop") < 0 {
		t.Fatal("pod.Stop should still be called even when DeltaCapture=false")
	}
}

// R3: Foreign-NodeID orphan is skipped (neither captured nor stopped).
func TestReapOrphansForeignNodeSkipped(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{
		listManaged: []runtime.ManagedPod{
			{SpawnID: "foreign-spawn", AgentID: "ag-foreign", NodeID: "other-node", Generation: 1},
		},
	}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	})

	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if fb.capturedRef != "" {
		t.Fatalf("must NOT capture foreign node's pod; capturedRef=%q", fb.capturedRef)
	}
	if fb.stopped != nil {
		t.Fatalf("must NOT stop foreign node's pod; stopped=%v", fb.stopped)
	}
}

// R4: A spawn that is still in the store is not treated as orphan.
func TestReapOrphansSkipsLiveSpawn(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{
		listManaged: []runtime.ManagedPod{
			{SpawnID: "live-spawn", AgentID: "ag-live", NodeID: "node-9", Generation: 1},
		},
	}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		NodeID:       "node-9",
		DeltaCapture: true,
	})
	// Insert into store to simulate a live spawn.
	m.store.Put(&Spawn{ID: "live-spawn", AgentID: "ag-live"})

	if err := m.ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if fb.capturedRef != "" {
		t.Fatalf("must NOT capture a still-live spawn; capturedRef=%q", fb.capturedRef)
	}
	if len(fb.ops) != 0 {
		t.Fatalf("no ops expected for live spawn; ops=%v", fb.ops)
	}
}
