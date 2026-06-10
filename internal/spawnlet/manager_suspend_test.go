package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Manager.Suspend tears the pod down exactly like Stop (node-local journal record persisted, spawn
// dropped from the store) but RETURNS the per-mount persist markers from the journal final snapshot,
// so the CP can record them. A journaled mount yields its pinned manifest id as the marker.
func TestManagerSuspendReturnsMarkers(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	app := writeJournalApp(t)
	fj := newFakeJournal("manifest-abc")
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, stateDir)

	sp, err := m.Create(ctx, "spj", app, "model", "", "", 3)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	markers, err := m.Suspend(ctx, sp.ID)
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(markers) != 1 || markers["main"] != "manifest-abc" {
		t.Fatalf("markers = %v, want {main: manifest-abc}", markers)
	}
	// Shared teardown: the node-local journal record is persisted (same as Stop)...
	if _, err := os.Stat(filepath.Join(stateDir, "spj.json")); err != nil {
		t.Fatalf("suspend did not persist the node-local journal record: %v", err)
	}
	// ...and the spawn is dropped from the store.
	if _, live := m.Store().Get(sp.ID); live {
		t.Fatal("suspended spawn must be dropped from the store")
	}

	// Unknown spawn -> error, nil markers.
	if mk, err := m.Suspend(ctx, "ghost"); err == nil || mk != nil {
		t.Fatalf("Suspend(ghost) = %v, %v; want error + nil markers", mk, err)
	}
}

// A scratch-only spawn (no journaled mounts) suspends with an EMPTY marker set — no journal record,
// nothing to persist — but still tears down cleanly.
func TestManagerSuspendScratchOnlyNoMarkers(t *testing.T) {
	ctx := context.Background()
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	sp, err := m.Create(ctx, "sps", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	markers, err := m.Suspend(ctx, sp.ID)
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(markers) != 0 {
		t.Fatalf("scratch-only suspend markers = %v, want empty", markers)
	}
	if _, live := m.Store().Get(sp.ID); live {
		t.Fatal("suspended spawn must be dropped from the store")
	}
}
