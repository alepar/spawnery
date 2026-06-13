package spawnlet

// manager_squash_test.go: tests for delta depth tracking + SQUASH-NEEDED surface (spec §3, task .12).
//
// Test matrix:
//   SQ1: DeltaDepth increments on each successful Suspend.
//   SQ2: DeltaDepth persists across a simulated node restart (Create reloads depth).
//   SQ3: SQUASH-NEEDED callback fires at DeltaSquashDepth threshold.
//   SQ4: SQUASH-NEEDED does NOT fire below threshold.

import (
	"context"
	"testing"
)

// SQ1: DeltaDepth increments on each successful Suspend.
func TestDeltaDepthIncrements(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	dataDir := t.TempDir()
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataDir,
		DeltaCapture:     true,
		DeltaSquashDepth: 10,
	}))

	app := writeApp(t)
	for i := 1; i <= 3; i++ {
		// Each iteration: create, suspend (which increments depth), then check state file.
		sp, err := m.Create(ctx, "sp-depth", app, "model", "", "", uint64(i))
		if err != nil {
			t.Fatalf("Create iter=%d: %v", i, err)
		}
		if sp.DeltaDepth != i-1 {
			t.Fatalf("iter=%d: DeltaDepth at Create = %d, want %d", i, sp.DeltaDepth, i-1)
		}
		if _, err := m.Suspend(ctx, sp.ID); err != nil {
			t.Fatalf("Suspend iter=%d: %v", i, err)
		}
		// Verify the state file was written with the correct depth.
		ds := &deltaStateStore{dir: dataDir + "/delta-state"}
		rec, found, err := ds.Load("sp-depth")
		if err != nil || !found {
			t.Fatalf("delta state not found after Suspend iter=%d: err=%v found=%v", i, err, found)
		}
		if rec.Depth != i {
			t.Fatalf("iter=%d: saved depth = %d, want %d", i, rec.Depth, i)
		}
	}
}

// SQ2: DeltaDepth persists across a simulated node restart (Create reloads depth from deltaState).
func TestDeltaDepthResumesContinuation(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	dataDir := t.TempDir()

	newMgr := func() *Manager {
		return noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
			AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataDir,
			DeltaCapture:     true,
			DeltaSquashDepth: 10,
		}))
	}

	app := writeApp(t)

	// First session: create + suspend (depth goes to 1).
	m1 := newMgr()
	sp1, err := m1.Create(ctx, "sp-resume-depth", app, "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sp1.DeltaDepth != 0 {
		t.Fatalf("fresh create depth = %d, want 0", sp1.DeltaDepth)
	}
	if _, err := m1.Suspend(ctx, sp1.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Simulate node restart: new Manager instance (empty in-mem store).
	m2 := newMgr()
	sp2, err := m2.Create(ctx, "sp-resume-depth", app, "model", "", "", 2)
	if err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if sp2.DeltaDepth != 1 {
		t.Fatalf("resumed depth = %d, want 1 (continuation from previous session)", sp2.DeltaDepth)
	}

	// Suspend again: depth should go to 2.
	if _, err := m2.Suspend(ctx, sp2.ID); err != nil {
		t.Fatalf("Suspend 2: %v", err)
	}
	ds := &deltaStateStore{dir: dataDir + "/delta-state"}
	rec, _, _ := ds.Load("sp-resume-depth")
	if rec.Depth != 2 {
		t.Fatalf("depth after second suspend = %d, want 2", rec.Depth)
	}
}

// SQ3: SQUASH-NEEDED callback fires at DeltaSquashDepth threshold.
func TestSquashNeededCallbackFires(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	dataDir := t.TempDir()

	var squashCalls []string
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataDir,
		DeltaCapture:     true,
		DeltaSquashDepth: 2, // low threshold for testing
	}))
	m.squashNeeded = func(id string, depth int) {
		squashCalls = append(squashCalls, id)
	}

	app := writeApp(t)
	for i := 1; i <= 3; i++ {
		sp, err := m.Create(ctx, "sp-squash", app, "model", "", "", uint64(i))
		if err != nil {
			t.Fatalf("Create iter=%d: %v", i, err)
		}
		if _, err := m.Suspend(ctx, sp.ID); err != nil {
			t.Fatalf("Suspend iter=%d: %v", i, err)
		}
	}

	// Should have fired once at depth=2 and once at depth=3.
	if len(squashCalls) < 2 {
		t.Fatalf("squashNeeded fired %d times, want ≥2 (at depths 2 and 3)", len(squashCalls))
	}
	for _, id := range squashCalls {
		if id != "sp-squash" {
			t.Fatalf("squashNeeded called with unexpected id %q", id)
		}
	}
}

// SQ4: SQUASH-NEEDED does NOT fire when depth stays below threshold.
func TestSquashNeededNotFiredBelowThreshold(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}

	var squashCalls int
	m := noScrub(NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:     true,
		DeltaSquashDepth: 10, // high threshold
	}))
	m.squashNeeded = func(_ string, _ int) {
		squashCalls++
	}

	app := writeApp(t)
	for i := 1; i <= 5; i++ {
		sp, err := m.Create(ctx, "sp-noquash", app, "model", "", "", uint64(i))
		if err != nil {
			t.Fatalf("Create iter=%d: %v", i, err)
		}
		if _, err := m.Suspend(ctx, sp.ID); err != nil {
			t.Fatalf("Suspend iter=%d: %v", i, err)
		}
	}

	if squashCalls != 0 {
		t.Fatalf("squashNeeded fired %d times, want 0 (below threshold of 10)", squashCalls)
	}
}
