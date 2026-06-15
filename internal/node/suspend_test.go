package node

import (
	"context"
	"errors"
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
// without the real Kopia stack. When finalErr is set, FinalSnapshot returns that error instead,
// simulating a suspend gate failure (e.g. journal sink unreachable). When putArtifactErr is set,
// PutArtifact returns that error, simulating a rootfs artifact store failure (FinishSuspend path).
type fakeNodeJournal struct {
	finalID        journal.ManifestID
	finalErr       error
	putArtifactErr error
}

func (f *fakeNodeJournal) RequestSnapshot(context.Context, string, uint64, journal.Mount) {}
func (f *fakeNodeJournal) FinalSnapshot(_ context.Context, _ string, _ uint64, mounts []journal.Mount) (map[string]journal.ManifestID, error) {
	if f.finalErr != nil {
		return nil, f.finalErr
	}
	out := map[string]journal.ManifestID{}
	for _, mt := range mounts {
		out[mt.Name] = f.finalID
	}
	return out, nil
}
func (f *fakeNodeJournal) WarmSnapshot(_ context.Context, _ string, _ uint64, mounts []journal.Mount) (map[string]journal.ManifestID, error) {
	out := map[string]journal.ManifestID{}
	for _, mt := range mounts {
		out[mt.Name] = f.finalID
	}
	return out, nil
}
func (f *fakeNodeJournal) Restore(context.Context, string, string, journal.ManifestID, string) error {
	return nil
}
func (f *fakeNodeJournal) RestoreGeneration(context.Context, string, uint64, string, journal.ManifestID, string) error {
	return nil
}
func (f *fakeNodeJournal) LatestForGeneration(context.Context, string, string, uint64) (journal.ManifestID, error) {
	return "", nil
}
func (f *fakeNodeJournal) PutArtifact(_ context.Context, spawnID string, generation uint64, desc journal.ArtifactDescriptor, r io.Reader) (journal.ArtifactDescriptor, error) {
	_, _ = io.Copy(io.Discard, r)
	if f.putArtifactErr != nil {
		return journal.ArtifactDescriptor{}, f.putArtifactErr
	}
	desc.SpawnID = spawnID
	desc.Generation = generation
	if desc.ArtifactID == "" {
		desc.ArtifactID = "artifact-test"
	}
	return desc, nil
}
func (f *fakeNodeJournal) GetArtifact(_ context.Context, spawnID string, generation uint64, artifactID string, w io.Writer) (journal.ArtifactDescriptor, error) {
	_, _ = w.Write([]byte("artifact-test"))
	return journal.ArtifactDescriptor{
		SpawnID: spawnID, Generation: generation, ArtifactID: artifactID,
		BaseImageDigest: "agent@sha256:base", Format: journal.ArtifactFormatOCILayout,
	}, nil
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
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{finalID: "manifest-abc"}, t.TempDir())
	return mgr
}

// newGateFailManager creates a manager whose journal FinalSnapshot always returns an error, so
// SnapshotForSuspend (the suspend gate) fails. This drives the fail-closed ACTIVE-on-gate-failure path.
func newGateFailManager(t *testing.T, be *scriptedPodBackend) *spawnlet.Manager {
	t.Helper()
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{finalErr: errors.New("journal sink unreachable")}, t.TempDir())
	return mgr
}

// newFinishFailManager creates a manager whose journal gate succeeds but PutArtifact fails,
// so FinishSuspend (called with captureRootfsArtifact=true) returns an error. This drives the
// post-gate teardown error path where sessions are already reaped.
func newFinishFailManager(t *testing.T, be *scriptedPodBackend) *spawnlet.Manager {
	t.Helper()
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{
		finalID:        "manifest-abc",
		putArtifactErr: errors.New("artifact store unavailable"),
	}, t.TempDir())
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

func TestSuspendWithRootfsCaptureEmitsArtifacts(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newJournaledManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp-rootfs", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 7})
	if got := lastPhase(fs.phasesFor("sp-rootfs")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("phase before suspend = %v, want ACTIVE", got)
	}

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{Suspend: &nodev1.Suspend{
		SpawnId: "sp-rootfs", Generation: 7, CaptureRootfsArtifact: true,
	}}})

	waitFor(t, "SuspendComplete", func() bool { return lastSuspendComplete(fs) != nil })
	sc := lastSuspendComplete(fs)
	if len(sc.RootfsArtifacts) != 1 {
		t.Fatalf("rootfs artifacts = %+v, want one", sc.RootfsArtifacts)
	}
	art := sc.RootfsArtifacts[0]
	if art.ArtifactId == "" || art.Generation != 7 || art.BaseImageDigest == "" {
		t.Fatalf("rootfs artifact descriptor = %+v", art)
	}
}

func TestStartSpawnRestoresPinnedRootfsArtifacts(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newJournaledManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{
		SpawnId: "sp-target", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 8,
		BaseImageDigest:        "agent@sha256:base",
		RootfsSourceGeneration: 7,
		RootfsArtifacts: []*nodev1.RootfsArtifact{{
			ArtifactId: "artifact-rootfs-gen7", Generation: 7, BaseImageDigest: "agent@sha256:base",
			Format: journal.ArtifactFormatOCILayout,
		}},
	})

	if got := lastPhase(fs.phasesFor("sp-target")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("phase after start = %v, want ACTIVE", got)
	}
	be.mu.Lock()
	imported, base := be.imported, be.importBase
	be.mu.Unlock()
	if !imported || base != "agent@sha256:base" {
		t.Fatalf("rootfs import imported=%v base=%q, want true/agent@sha256:base", imported, base)
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

// TestSuspendGateFailureStaysActive verifies the fail-closed gate path (spec §5): when
// SnapshotForSuspend fails (journal sink unreachable), the node emits SuspendComplete{Error}
// and keeps the spawn ACTIVE — sessions are NOT reaped, the pod is NOT stopped, the slot
// is NOT released.
func TestSuspendGateFailureStaysActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newGateFailManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 3})
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("phase before suspend = %v, want ACTIVE", got)
	}

	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{Suspend: &nodev1.Suspend{SpawnId: "sp1", Generation: 3}}})

	waitFor(t, "SuspendComplete with error", func() bool {
		sc := lastSuspendComplete(fs)
		return sc != nil && sc.Error != ""
	})

	sc := lastSuspendComplete(fs)
	if sc.Error == "" {
		t.Fatal("SuspendComplete.Error must be non-empty on gate failure")
	}
	if sc.SpawnId != "sp1" {
		t.Fatalf("SuspendComplete.SpawnId = %q, want sp1", sc.SpawnId)
	}
	if sc.Generation != 3 {
		t.Fatalf("SuspendComplete.Generation = %d, want 3", sc.Generation)
	}
	if len(sc.Markers) != 0 {
		t.Fatalf("SuspendComplete.Markers = %v, want empty on gate failure", sc.Markers)
	}
	if len(sc.RootfsArtifacts) != 0 {
		t.Fatalf("SuspendComplete.RootfsArtifacts = %v, want empty on gate failure", sc.RootfsArtifacts)
	}

	// Status must return to ACTIVE (not SUSPENDED or ERROR).
	waitFor(t, "ACTIVE phase after gate failure", func() bool {
		return lastPhase(fs.phasesFor("sp1")) == nodev1.SpawnPhase_ACTIVE
	})
	if hasPhase(fs.phasesFor("sp1"), nodev1.SpawnPhase_SUSPENDED) {
		t.Fatal("spawn must NOT reach SUSPENDED on gate failure")
	}

	// Sessions must NOT be reaped — pump still registered, active count unchanged.
	a.mu.Lock()
	n, act := len(a.pumps), a.active
	a.mu.Unlock()
	if n != 1 || act != 1 {
		t.Fatalf("pumps=%d active=%d, want 1/1 after gate failure (sessions must not be reaped)", n, act)
	}

	// Pod must NOT be stopped.
	if be.wasStopped() {
		t.Fatal("pod must NOT be stopped on gate failure")
	}

	// Spawn must still be live in the manager store.
	if _, live := mgr.SpawnGeneration("sp1"); !live {
		t.Fatal("spawn must remain live in manager store after gate failure")
	}
}

// TestSuspendFinishFailureReleasesSlot verifies that when the gate (SnapshotForSuspend) succeeds
// but FinishSuspend fails (e.g. rootfs artifact store unavailable), the capacity slot IS released.
// Sessions are reaped before FinishSuspend runs, so the spawn is dead from the attacher's
// perspective regardless of the finish outcome — keeping the slot would permanently leak capacity.
func TestSuspendFinishFailureReleasesSlot(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newFinishFailManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)
	ctx := context.Background()

	a.startSpawn(ctx, &nodev1.StartSpawn{SpawnId: "sp1", AppRef: writeNodeJournalApp(t), Model: "m", Generation: 4})
	if got := lastPhase(fs.phasesFor("sp1")); got != nodev1.SpawnPhase_ACTIVE {
		t.Fatalf("phase before suspend = %v, want ACTIVE", got)
	}

	// Trigger suspend with rootfs capture so FinishSuspend's PutArtifact path runs and fails.
	a.handle(ctx, &nodev1.CPMessage{Msg: &nodev1.CPMessage_Suspend{Suspend: &nodev1.Suspend{
		SpawnId: "sp1", Generation: 4, CaptureRootfsArtifact: true,
	}}})

	// Wait for the ERROR status that FinishSuspend failure emits.
	waitFor(t, "ERROR phase after finish failure", func() bool {
		return lastPhase(fs.phasesFor("sp1")) == nodev1.SpawnPhase_ERROR
	})

	// Capacity slot MUST be released — sessions were reaped before FinishSuspend.
	a.mu.Lock()
	pumps, act := len(a.pumps), a.active
	a.mu.Unlock()
	if act != 0 {
		t.Fatalf("active=%d, want 0 after finish failure (slot must be released)", act)
	}
	if pumps != 0 {
		t.Fatalf("pumps=%d, want 0 after finish failure (sessions must be reaped)", pumps)
	}

	// Must NOT reach SUSPENDED.
	if hasPhase(fs.phasesFor("sp1"), nodev1.SpawnPhase_SUSPENDED) {
		t.Fatal("spawn must NOT reach SUSPENDED on finish failure")
	}
}
