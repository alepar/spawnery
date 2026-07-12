package spawnlet

// manager_scrub_guard_test.go: the delta-scrub mount guard at the manager level (sp-2tx8.2.2, spec
// §4.2). The scrub runs BEFORE the mount snapshot, so a DeltaScrubPaths entry that overlaps a
// mount would delete the user's data and then have the deletion snapshotted as authoritative.
//
// Test matrix:
//   G1: a scrub path overlapping a mount is NOT handed to scrubFn; disjoint paths still are.
//   G2: mount data SURVIVES an overlapping DeltaScrubPaths entry (end-to-end through Suspend).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// G1: /app/data is a mount (writeApp declares name=main path=data); it must be filtered out of the
// scrub path list, while /tmp — which touches nothing mounted — must still be scrubbed.
func TestScrubGuardFiltersMountOverlappingPaths(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture:    true,
		DeltaScrubPaths: []string{"/app/data", "/app/data/node_modules", "/tmp"},
	})

	var gotPaths []string
	scrubCalled := false
	m.scrubFn = func(_ context.Context, _ string, paths []string) error {
		scrubCalled = true
		gotPaths = append([]string(nil), paths...)
		return nil
	}

	sp, err := m.Create(ctx, "sp-guard", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if !scrubCalled {
		t.Fatal("scrubFn was not called (the guard must filter paths, not disable the scrub)")
	}
	want := []string{"/tmp"}
	if !equalPaths(gotPaths, want) {
		t.Fatalf("scrub paths = %v, want %v (mount-overlapping paths must be skipped)", gotPaths, want)
	}
}

// G2: the seeded file in the /app/data mount survives a DeltaScrubPaths entry that overlaps the
// mount. The scrubFn here is a stand-in for the real `rm -rf` exec: it deletes the host dir behind
// any container path it is handed. Pre-guard, "/app/data" reaches it and the mount is destroyed.
// The survival check runs at CAPTURE time (the fake's captureHook), because teardown's
// Scratch.Finalize removes the host dir afterwards regardless.
func TestScrubGuardMountDataSurvives(t *testing.T) {
	ctx := context.Background()
	fb := &captureHookBackend{Backend: fakeBackend(t)}
	dataRoot := t.TempDir()
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: dataRoot,
		DeltaCapture:    true,
		DeltaScrubPaths: []string{"/app/data", "/tmp"},
	})

	sp, err := m.Create(ctx, "sp-guard-data", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sp.MountDirs) != 1 {
		t.Fatalf("want 1 mount dir, got %v", sp.MountDirs)
	}
	mountHostDir := sp.MountDirs[0]
	seeded := filepath.Join(mountHostDir, "README.md") // writeApp seeds README.md into the mount
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("precondition: seeded mount file missing: %v", err)
	}

	// Stand-in for the real `rm -rf` exec inside the container: /app/data is bind-mounted from
	// mountHostDir, so scrubbing it destroys the host dir's contents.
	var scrubbed []string
	m.scrubFn = func(_ context.Context, _ string, paths []string) error {
		scrubbed = append([]string(nil), paths...)
		for _, p := range paths {
			if p == "/app/data" {
				if err := os.RemoveAll(mountHostDir); err != nil {
					t.Errorf("fake scrub: %v", err)
				}
			}
		}
		return nil
	}

	// Snapshot the mount's state at the moment the rootfs delta is captured.
	var survivedAtCapture bool
	fb.captureHook = func() {
		_, err := os.Stat(seeded)
		survivedAtCapture = err == nil
	}

	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if !survivedAtCapture {
		t.Fatalf("mount data was destroyed by the scrub (paths handed to scrubFn: %v); "+
			"a DeltaScrubPaths entry at or under a mount must be skipped", scrubbed)
	}
	// The guard must be surgical, not a blanket disable: /tmp is still scrubbed.
	if !equalPaths(scrubbed, []string{"/tmp"}) {
		t.Fatalf("scrub paths = %v, want [/tmp]", scrubbed)
	}
}
