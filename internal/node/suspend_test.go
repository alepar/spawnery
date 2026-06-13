package node

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

// fakeNodeJournal is a journal.JournalManager double for the node-side suspend tests: FinalSnapshot
// returns a canned pinned manifest id per journaled mount, so a Suspend yields per-mount markers
// without the real Kopia stack.
type fakeNodeJournal struct{ finalID journal.ManifestID }

func (f *fakeNodeJournal) RequestSnapshot(context.Context, string, uint64, journal.Mount) {}
func (f *fakeNodeJournal) FinalSnapshot(_ context.Context, _ string, _ uint64, mounts []journal.Mount) (map[string]journal.ManifestID, error) {
	out := map[string]journal.ManifestID{}
	for _, mt := range mounts {
		out[mt.Name] = f.finalID
	}
	return out, nil
}
func (f *fakeNodeJournal) Restore(context.Context, string, string, journal.ManifestID, string) error {
	return nil
}
func (f *fakeNodeJournal) LatestForGeneration(context.Context, string, string, uint64) (journal.ManifestID, error) {
	return "", nil
}
func (f *fakeNodeJournal) PutArtifact(_ context.Context, spawnID string, generation uint64, desc journal.ArtifactDescriptor, r io.Reader) (journal.ArtifactDescriptor, error) {
	_, _ = io.Copy(io.Discard, r)
	desc.SpawnID = spawnID
	desc.Generation = generation
	if desc.ArtifactID == "" {
		desc.ArtifactID = "artifact-test"
	}
	return desc, nil
}
func (f *fakeNodeJournal) GetArtifact(_ context.Context, spawnID string, generation uint64, artifactID string, w io.Writer) (journal.ArtifactDescriptor, error) {
	_, _ = w.Write([]byte("artifact-test"))
	return journal.ArtifactDescriptor{SpawnID: spawnID, Generation: generation, ArtifactID: artifactID}, nil
}
func (f *fakeNodeJournal) ListArtifacts(context.Context, string, uint64, string) ([]journal.ArtifactDescriptor, error) {
	return nil, nil
}
func (f *fakeNodeJournal) QuickMaintenance(context.Context, string) error { return nil }
func (f *fakeNodeJournal) Close(context.Context, string) error            { return nil }

// writeNodeJournalApp writes an app manifest whose single mount opts into the node-local durability
// class, so the journal seams engage and the mount is captured by FinalSnapshot.
func writeNodeJournalApp(t *testing.T) string {
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

// lastSuspendComplete returns the most recent SuspendComplete the attacher sent (nil if none).
func lastSuspendComplete(f *fakeCPStream) *nodev1.SuspendComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if sc := f.sent[i].GetSuspendComplete(); sc != nil {
			return sc
		}
	}
	return nil
}

func newJournaledManager(t *testing.T, be *scriptedPodBackend) *spawnlet.Manager {
	t.Helper()
	mgr := newGooseManager(t, be)
	mgr.SetJournal(&fakeNodeJournal{finalID: "manifest-abc"}, t.TempDir())
	return mgr
}

// handle(Suspend) for the live generation: the node reaps sessions, persists the mounts, emits
// SuspendComplete echoing the generation + carrying the per-mount markers from the journal final
// snapshot, ends with a SUSPENDED phase, releases the capacity slot, and tears the pod down.
func TestSuspendEmitsMarkersAndSuspended(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newJournaledManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 2})
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("phase before suspend = %v, want ACTIVE", got)
	}

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{Suspend: &nodev1.Suspend{SpawnId: "sp1", Generation: 2}}})

	waitFor(t, "SuspendComplete", func() bool { return lastSuspendComplete(fs) != nil })
	sc := lastSuspendComplete(fs)
	if sc.SpawnId != "sp1" || sc.Generation != 2 {
		t.Fatalf("SuspendComplete spawn/gen = %q/%d, want sp1/2", sc.SpawnId, sc.Generation)
	}
	if len(sc.Markers) != 1 || sc.Markers[0].Name != "main" || sc.Markers[0].Marker != "manifest-abc" {
		t.Fatalf("markers = %+v, want one {main: manifest-abc}", sc.Markers)
	}
	waitFor(t, "SUSPENDED phase", func() bool { return lastPhase(fs.phasesFor("sp1")) == nodev1.SpawnPhase_SUSPENDED })
	if !be.wasStopped() {
		t.Fatal("suspend must tear the pod down (mgr.Suspend -> pod.Stop)")
	}
	a.mu.Lock()
	n, act := len(a.pumps), a.active
	a.mu.Unlock()
	if n != 0 || act != 0 {
		t.Fatalf("pumps=%d active=%d, want 0/0 after suspend", n, act)
	}
	if _, live := mgr.SpawnGeneration("sp1"); live {
		t.Fatal("suspended spawn must be dropped from the manager store")
	}
}

// A Suspend carrying a generation BELOW the live pod's is a stale-episode message: the node drops it
// (no SuspendComplete, pod still running, slot still held) — mirrors the Stop/SetModel fence.
func TestSuspendStaleGenerationDropped(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newJournaledManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 5})
	defer a.stopSpawn(ctx, "sp1")

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{Suspend: &nodev1.Suspend{SpawnId: "sp1", Generation: 4}}})

	if sc := lastSuspendComplete(fs); sc != nil {
		t.Fatalf("stale Suspend must not emit SuspendComplete, got %+v", sc)
	}
	if hasPhase(fs.phasesFor("sp1"), nodev1.SpawnPhase_SUSPENDED) {
		t.Fatal("stale Suspend must not report SUSPENDED")
	}
	if be.wasStopped() {
		t.Fatal("stale Suspend must not tear the pod down")
	}
	if _, live := mgr.SpawnGeneration("sp1"); !live {
		t.Fatal("stale Suspend must leave the spawn running")
	}
}
