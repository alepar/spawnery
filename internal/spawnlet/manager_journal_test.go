package spawnlet

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"spawnery/internal/storage/journal"
)

// fakeJournal is a JournalManager double: FinalSnapshot returns a canned pinned
// manifest id per mount, and Restore records what it was asked to restore — so a
// test can assert the node-local suspend→resume round trip (save on Stop, load +
// restore on the next Create) without the real Kopia stack.
type fakeJournal struct {
	mu          sync.Mutex
	finalID     journal.ManifestID
	restoreSeen map[string]journal.ManifestID // mountName -> manifest id Restore was called with
	requested   chan journal.Mount            // each RequestSnapshot pushes the mount (drops if full)
}

func newFakeJournal(id journal.ManifestID) *fakeJournal {
	return &fakeJournal{finalID: id, restoreSeen: map[string]journal.ManifestID{}, requested: make(chan journal.Mount, 256)}
}

func (f *fakeJournal) RequestSnapshot(_ context.Context, _ string, _ uint64, mt journal.Mount) {
	select {
	case f.requested <- mt:
	default:
	}
}

func (f *fakeJournal) FinalSnapshot(_ context.Context, _ string, _ uint64, mounts []journal.Mount) (map[string]journal.ManifestID, error) {
	out := map[string]journal.ManifestID{}
	for _, mt := range mounts {
		out[mt.Name] = f.finalID
	}
	return out, nil
}

func (f *fakeJournal) Restore(_ context.Context, _, mountName string, id journal.ManifestID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreSeen[mountName] = id
	return nil
}

func (f *fakeJournal) LatestForGeneration(context.Context, string, string, uint64) (journal.ManifestID, error) {
	return "", nil
}
func (f *fakeJournal) QuickMaintenance(context.Context, string) error { return nil }
func (f *fakeJournal) Close(context.Context, string) error            { return nil }

// writeJournalApp is writeApp with the mount opted into the node-local
// durability class, so the journal seams engage.
func writeJournalApp(t *testing.T) string {
	t.Helper()
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "spawneryapp.yml"), []byte(`
id: spawnery/journaled
storage:
  mounts:
    - name: main
      path: data
      seed: seed
      durability: node-local
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "seed", "README.md"), []byte("QUOKKA-4417"), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

// A node-local journaled spawn: the FIRST Create has no record (no restore); on
// Stop the manager persists the pinned manifest id durably on the node; the
// SECOND Create (same id = same-node resume) loads that record and restores the
// pinned manifest — all without any CP protocol.
func TestNodeLocalSuspendResumeRestoresPinnedManifest(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	app := writeJournalApp(t)

	newMgr := func() (*Manager, *fakeJournal) {
		fj := newFakeJournal("manifest-abc")
		m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
			AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		})
		m.SetJournal(fj, stateDir)
		return m, fj
	}

	// First create: fresh, no record yet -> Restore must NOT be called.
	m1, fj1 := newMgr()
	sp, err := m1.Create(ctx, "spj", app, "model", "", "", 0)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if len(fj1.restoreSeen) != 0 {
		t.Fatalf("fresh create must not restore, got %v", fj1.restoreSeen)
	}

	// Suspend: FinalSnapshot returns the pinned id, which the manager persists.
	if err := m1.Stop(ctx, sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "spj.json")); err != nil {
		t.Fatalf("suspend did not persist the node-local journal record: %v", err)
	}

	// Resume: a new manager (fresh in-memory state, same node state dir) creating
	// the same id loads the record and restores the pinned manifest.
	m2, fj2 := newMgr()
	if _, err := m2.Create(ctx, "spj", app, "model", "", "", 0); err != nil {
		t.Fatalf("resume Create: %v", err)
	}
	got, ok := fj2.restoreSeen["main"]
	if !ok {
		t.Fatal("resume did not restore the journaled mount")
	}
	if got != "manifest-abc" {
		t.Fatalf("restored manifest = %q, want manifest-abc (the pinned id from suspend)", got)
	}
}

// A scratch-only spawn (no journaled mounts, no journaler) must be wholly
// unaffected: no record written, no restore attempted.
func TestScratchOnlySpawnLeavesNoJournalState(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	fj := newFakeJournal("x")
	m.SetJournal(fj, stateDir)

	sp, err := m.Create(ctx, "sps", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Stop(ctx, sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "sps.json")); !os.IsNotExist(err) {
		t.Fatalf("scratch-only spawn must not write a journal record (err=%v)", err)
	}
	if len(fj.restoreSeen) != 0 {
		t.Fatalf("scratch-only spawn must not restore, got %v", fj.restoreSeen)
	}
	// And no continuous watcher fired (scratch-only spawns get none).
	select {
	case mt := <-fj.requested:
		t.Fatalf("scratch-only spawn must not request snapshots, got %v", mt)
	default:
	}
}

// The continuous watcher (sp-u53.5.2) started on Create drives RequestSnapshot
// when the journaled mount's host dir changes — the headline phase-② wiring.
func TestWatcherDrivesRequestSnapshotOnWrite(t *testing.T) {
	ctx := context.Background()
	app := writeJournalApp(t)
	fj := newFakeJournal("manifest-x")
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, "spw", app, "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = m.Stop(ctx, sp.ID) }()

	if len(sp.JournalMounts) != 1 {
		t.Fatalf("expected 1 journaled mount, got %d", len(sp.JournalMounts))
	}
	hostDir := sp.JournalMounts[0].HostDir

	// Write into the watched mount dir; the watcher must drive a RequestSnapshot.
	if err := os.WriteFile(filepath.Join(hostDir, "change.txt"), []byte("EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case mt := <-fj.requested:
		if mt.Name != "main" {
			t.Fatalf("RequestSnapshot for mount %q, want main", mt.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not drive RequestSnapshot after a write into the journaled mount")
	}
}
