package spawnlet

// manager_gc_test.go: hermetic tests for the GC-on-Delete path (spec §4, task .12).
//
// Test matrix:
//   G1: Delete calls ReleaseDelta and empties the store.
//   G2: Suspend does NOT call ReleaseDelta (delta image preserved for same-node resume).
//   G3: Stop does NOT call ReleaseDelta (delta image preserved for same-node resume).
//   G4: Delete purges the durable delta state file; Suspend creates it; Stop leaves it alone.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// noScrub disables the exec-based delta scrub so these hermetic tests never shell out to
// Docker (the constructor defaults DeltaScrubPaths, so DeltaCapture+Suspend would otherwise
// exec `docker exec <agentID> rm -rf ...` against the fake's non-existent container).
func noScrub(m *Manager) *Manager {
	m.scrubFn = func(context.Context, string, []string) error { return nil }
	return m
}

// G1: Delete triggers ReleaseDelta + removes spawn from store.
func TestDeleteReleasesGC(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	dataDir := t.TempDir()
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataDir,
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp-gc", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := sp.ID

	if err := m.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// ReleaseDelta must have been called.
	if fb.releasedSpawn != id {
		t.Fatalf("ReleaseDelta not called with id=%s; got releasedSpawn=%q", id, fb.releasedSpawn)
	}
	// Spawn must be removed from store.
	if _, live := m.Store().Get(id); live {
		t.Fatal("spawn should be removed from store after Delete")
	}
}

// G2: Suspend does NOT call ReleaseDelta.
func TestSuspendDoesNotGC(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp-sus-gc", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if fb.releasedSpawn != "" {
		t.Fatalf("ReleaseDelta must NOT be called on Suspend; got releasedSpawn=%q", fb.releasedSpawn)
	}
}

// G3: Stop does NOT call ReleaseDelta.
func TestStopDoesNotGC(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	}))

	sp, err := m.Create(ctx, "sp-stop-gc", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.Stop(ctx, sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if fb.releasedSpawn != "" {
		t.Fatalf("ReleaseDelta must NOT be called on Stop; got releasedSpawn=%q", fb.releasedSpawn)
	}
}

// G4: Suspend creates the delta state file; Delete purges it.
func TestDeletePurgesDeltaStateFile(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	dataDir := t.TempDir()
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataDir,
		DeltaCapture: true,
	}))

	// Suspend writes the delta state file.
	sp, err := m.Create(ctx, "sp-state-purge", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := sp.ID

	if _, err := m.Suspend(ctx, id); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	stateFile := filepath.Join(dataDir, "delta-state", id+".delta.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("Suspend should have created delta state file %s: %v", stateFile, err)
	}

	// Re-create spawn so we can Delete it.
	if _, err := m.Create(ctx, id, writeApp(t), "model", "", "", 2); err != nil {
		t.Fatalf("re-Create: %v", err)
	}

	if err := m.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("Delete should have purged delta state file %s; stat err=%v", stateFile, err)
	}
}
