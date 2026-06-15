package spawnlet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/storage/journal"
)

type forkOpRecorder struct {
	mu  sync.Mutex
	ops []string
}

func (r *forkOpRecorder) add(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

func (r *forkOpRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

type recordingForkBackend struct {
	fakePodBackend
	rec        *forkOpRecorder
	unpauseErr error
}

func (b *recordingForkBackend) Pause(ctx context.Context, h *runtime.PodHandle) error {
	b.rec.add("pause-agent:" + h.SpawnID)
	return b.fakePodBackend.Pause(ctx, h)
}

func (b *recordingForkBackend) Unpause(ctx context.Context, h *runtime.PodHandle) error {
	b.rec.add("unpause-agent:" + h.SpawnID)
	if b.unpauseErr != nil {
		return b.unpauseErr
	}
	return b.fakePodBackend.Unpause(ctx, h)
}

func (b *recordingForkBackend) CaptureDeltaAs(ctx context.Context, h *runtime.PodHandle, targetSpawnID string) (string, error) {
	b.rec.add("capture-rootfs-as:" + targetSpawnID)
	return b.fakePodBackend.CaptureDeltaAs(ctx, h, targetSpawnID)
}

func (b *recordingForkBackend) ExportDelta(ctx context.Context, spawnID string, w io.Writer) error {
	b.rec.add("export-rootfs:" + spawnID)
	_, err := w.Write([]byte(runtime.DeltaTag(spawnID)))
	return err
}

type recordingForkJournal struct {
	rec            *forkOpRecorder
	finalErr       error
	finalErrOnCall int
	finalCalls     int
}

func (j *recordingForkJournal) RequestSnapshot(_ context.Context, spawnID string, gen uint64, _ journal.Mount) {
	j.rec.add(fmt.Sprintf("warm-final-snapshot:%s:%d", spawnID, gen))
}

func (j *recordingForkJournal) FinalSnapshot(_ context.Context, spawnID string, gen uint64, mounts []journal.Mount) (map[string]journal.ManifestID, error) {
	j.rec.add(fmt.Sprintf("final-snapshot:%s:%d", spawnID, gen))
	j.finalCalls++
	if j.finalErr != nil && (j.finalErrOnCall == 0 || j.finalErrOnCall == j.finalCalls) {
		return nil, j.finalErr
	}
	out := map[string]journal.ManifestID{}
	for _, mt := range mounts {
		out[mt.Name] = journal.ManifestID(fmt.Sprintf("%s-%s-gen%d", spawnID, mt.Name, gen))
	}
	return out, nil
}

func (j *recordingForkJournal) Restore(_ context.Context, spawnID, mountName string, id journal.ManifestID, hostDir string) error {
	j.rec.add(fmt.Sprintf("seed-fork-mount:%s:%s:%s:%s", spawnID, mountName, id, hostDir))
	return nil
}

func (j *recordingForkJournal) LatestForGeneration(context.Context, string, string, uint64) (journal.ManifestID, error) {
	return "", nil
}

func (j *recordingForkJournal) PutArtifact(_ context.Context, spawnID string, generation uint64, desc journal.ArtifactDescriptor, r io.Reader) (journal.ArtifactDescriptor, error) {
	j.rec.add(fmt.Sprintf("put-fork-rootfs-artifact:%s:%d", spawnID, generation))
	_, _ = io.Copy(io.Discard, r)
	desc.SpawnID = spawnID
	desc.Generation = generation
	if desc.ArtifactID == "" {
		desc.ArtifactID = "fork-rootfs"
	}
	return desc, nil
}

func (j *recordingForkJournal) GetArtifact(context.Context, string, uint64, string, io.Writer) (journal.ArtifactDescriptor, error) {
	return journal.ArtifactDescriptor{}, nil
}
func (j *recordingForkJournal) ListArtifacts(context.Context, string, uint64, string) ([]journal.ArtifactDescriptor, error) {
	return nil, nil
}
func (j *recordingForkJournal) QuickMaintenance(context.Context, string) error { return nil }
func (j *recordingForkJournal) Close(context.Context, string) error            { return nil }

func newForkTestManager(t *testing.T, rec *forkOpRecorder, j *recordingForkJournal) (*Manager, *recordingForkBackend) {
	t.Helper()
	fb := &recordingForkBackend{rec: rec}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		NodeID: "node-1", AgentImage: "agent:base", SidecarImage: "sidecar:base",
		DataRoot: t.TempDir(), DeltaCapture: true,
	})
	m.SetJournal(j, t.TempDir())
	m.forkSyncFn = func(context.Context) error {
		rec.add("sync-host")
		return nil
	}
	return m, fb
}

func putForkSource(t *testing.T, m *Manager, sourceID string, generation uint64) {
	t.Helper()
	srcDir := t.TempDir()
	m.store.Put(&Spawn{
		ID: sourceID, Generation: generation, AgentID: "ag-source", SidecarID: "sc-source",
		BaseImageDigest: "agent@sha256:base", LaunchImageRef: "agent:base",
		JournalMounts: []journal.Mount{{Name: "work", HostDir: srcDir, Class: journal.NodeLocal}},
	})
}

func TestForkSameNodeCapturesMountsAndRootfsUnderOnePause(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)
	putForkSource(t, m, "sp-source", 9)

	res, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID:    "sp-source",
		ForkSpawnID:      "sp-fork",
		SourceGeneration: 9,
		TargetGeneration: 1,
	})
	if err != nil {
		t.Fatalf("ForkSameNode: %v", err)
	}
	if res.MountPins["work"] != "sp-fork-work-gen1" {
		t.Fatalf("mount pins = %+v", res.MountPins)
	}
	if len(res.RootfsArtifacts) != 1 || res.RootfsArtifacts[0].Generation != 1 {
		t.Fatalf("rootfs artifacts = %+v", res.RootfsArtifacts)
	}
	jrec, ok, err := m.journalState.Load("sp-fork")
	if err != nil {
		t.Fatalf("load fork journal state: %v", err)
	}
	if !ok || jrec.Generation != 1 || jrec.Manifests["work"] != journal.ManifestID("sp-fork-work-gen1") {
		t.Fatalf("fork journal state = %+v ok=%v, want gen1 work manifest", jrec, ok)
	}

	want := []string{
		"final-snapshot:sp-source:9",
		"pause-agent:sp-source",
		"sync-host",
		"final-snapshot:sp-source:9",
		"capture-rootfs-as:sp-fork",
		"export-rootfs:sp-fork",
		"unpause-agent:sp-source",
		"seed-fork-mount:sp-source:work:sp-source-work-gen9:",
		"put-fork-rootfs-artifact:sp-fork:1",
		"final-snapshot:sp-fork:1",
	}
	assertForkOpsPrefix(t, rec.snapshot(), want)
}

func TestForkSameNodeFailsClosedWhenRequiredGenerationHoldIsUnwired(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, _ := newForkTestManager(t, rec, j)
	m.forkGenerationHoldRequired = true
	putForkSource(t, m, "sp-source", 9)

	_, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID:    "sp-source",
		ForkSpawnID:      "sp-fork",
		SourceGeneration: 9,
		TargetGeneration: 1,
		TransferSetID:    "ts-1",
	})
	if err == nil || !strings.Contains(err.Error(), "generation hold is required") {
		t.Fatalf("ForkSameNode error = %v, want generation hold requirement", err)
	}
	if ops := rec.snapshot(); len(ops) != 0 {
		t.Fatalf("fork must fail before snapshots/pause when hold is required but unwired, ops=%v", ops)
	}
}

func TestForkUnpauseIfPausedToleratesAlreadyRunning(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec}
	m, fb := newForkTestManager(t, rec, j)
	fb.unpauseErr = fmt.Errorf("container is not paused")
	putForkSource(t, m, "sp-source", 9)

	if err := m.UnpauseIfPaused(ctx, "sp-source", 9); err != nil {
		t.Fatalf("UnpauseIfPaused: %v", err)
	}
}

func TestForkCaptureFailureUnpausesAndRestartsWatchers(t *testing.T) {
	ctx := context.Background()
	rec := &forkOpRecorder{}
	j := &recordingForkJournal{rec: rec, finalErr: fmt.Errorf("snapshot failed"), finalErrOnCall: 2}
	m, fb := newForkTestManager(t, rec, j)
	putForkSource(t, m, "sp-source", 9)

	_, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID:    "sp-source",
		ForkSpawnID:      "sp-fork",
		SourceGeneration: 9,
		TargetGeneration: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("ForkSameNode error = %v, want snapshot failed", err)
	}
	if fb.unpauseCount == 0 {
		t.Fatal("failure must unpause the source")
	}
	if _, ok := m.store.Get("sp-source"); !ok {
		t.Fatal("failed fork capture must leave source in the store")
	}
	if bytes.Contains([]byte(strings.Join(rec.snapshot(), ",")), []byte("capture-rootfs-as")) {
		t.Fatalf("rootfs capture must not run after failed final snapshot, ops=%v", rec.snapshot())
	}
}

type fakeGenerationHolds struct {
	mu       sync.Mutex
	held     []string
	released int
}

func (f *fakeGenerationHolds) HoldGeneration(spawnID string, gen uint64, reason string) generationHold {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = append(f.held, fmt.Sprintf("%s:%d:%s", spawnID, gen, reason))
	return generationHoldFunc(func() {
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
	})
}

type generationHoldFunc func()

func (f generationHoldFunc) Release() { f() }

func TestForkSameNodeReleasesGenerationHoldOnSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		finalErr error
	}{
		{name: "success"},
		{name: "failure", finalErr: fmt.Errorf("snapshot failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rec := &forkOpRecorder{}
			j := &recordingForkJournal{rec: rec, finalErr: tc.finalErr}
			m, _ := newForkTestManager(t, rec, j)
			holds := &fakeGenerationHolds{}
			m.forkGenerationHold = holds.HoldGeneration
			putForkSource(t, m, "sp-source", 9)

			_, _ = m.ForkSameNode(ctx, ForkSameNodeRequest{
				SourceSpawnID:    "sp-source",
				ForkSpawnID:      "sp-fork",
				SourceGeneration: 9,
				TargetGeneration: 1,
				TransferSetID:    "ts-1",
			})

			holds.mu.Lock()
			defer holds.mu.Unlock()
			if len(holds.held) != 1 || !strings.Contains(holds.held[0], "sp-source:9:fork ts-1") {
				t.Fatalf("holds = %+v", holds.held)
			}
			if holds.released != 1 {
				t.Fatalf("released=%d want 1", holds.released)
			}
		})
	}
}

func assertForkOpsPrefix(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("ops too short\n got: %v\nwant: %v", got, want)
	}
	for i, w := range want {
		if strings.HasPrefix(w, "seed-fork-mount:") {
			if !strings.HasPrefix(got[i], w) {
				t.Fatalf("op %d = %q, want prefix %q\nall ops: %v", i, got[i], w, got)
			}
			continue
		}
		if !reflect.DeepEqual(got[i], w) {
			t.Fatalf("op %d = %q, want %q\nall ops: %v", i, got[i], w, got)
		}
	}
}
