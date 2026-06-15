package node

import (
	"context"
	"testing"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage/journal"
)

func lastForkSameNodeComplete(f *fakeCPStream) *nodev1.ForkSameNodeComplete {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if fc := f.sent[i].GetForkSameNodeComplete(); fc != nil {
			return fc
		}
	}
	return nil
}

func newForkNodeManager(t *testing.T, be *scriptedPodBackend) *spawnlet.Manager {
	t.Helper()
	mgr := spawnlet.NewManagerWithBackend(be, noopApplier{}, spawnlet.ManagerConfig{
		NodeID: "node-test", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	mgr.SetJournal(&fakeNodeJournal{finalID: "manifest-abc"}, t.TempDir())
	return mgr
}

func putForkNodeSource(t *testing.T, mgr *spawnlet.Manager, id string, gen uint64) {
	t.Helper()
	mgr.Store().Put(&spawnlet.Spawn{
		ID: id, Generation: gen, AgentID: "ag-source", SidecarID: "sc-source",
		BaseImageDigest: "agent@sha256:base", LaunchImageRef: "agent:base",
		JournalMounts: []journal.Mount{{Name: "main", HostDir: t.TempDir(), Class: journal.NodeLocal}},
	})
}

func TestForkSameNodeStaleGenerationDropped(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 8, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	if fc := lastForkSameNodeComplete(fs); fc != nil {
		t.Fatalf("stale ForkSameNode must not emit completion, got %+v", fc)
	}
}

func TestForkSameNodeEmitsCompletion(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	putForkNodeSource(t, mgr, "sp-source", 9)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "sp-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkSameNodeComplete", func() bool { return lastForkSameNodeComplete(fs) != nil })
	fc := lastForkSameNodeComplete(fs)
	if fc.GetError() != "" {
		t.Fatalf("ForkSameNodeComplete error = %q", fc.GetError())
	}
	if fc.GetSourceSpawnId() != "sp-source" || fc.GetForkSpawnId() != "sp-fork" || fc.GetTransferSetId() != "ts-1" {
		t.Fatalf("ForkSameNodeComplete ids = %+v", fc)
	}
	if len(fc.GetMounts()) != 1 || fc.GetMounts()[0].GetName() != "main" {
		t.Fatalf("mounts = %+v", fc.GetMounts())
	}
	if len(fc.GetRootfsArtifacts()) != 1 || fc.GetRootfsArtifacts()[0].GetGeneration() != 1 {
		t.Fatalf("rootfs artifacts = %+v", fc.GetRootfsArtifacts())
	}
}

func TestForkSameNodeFailureCompletionDoesNotMarkForkActive(t *testing.T) {
	be := &scriptedPodBackend{script: scriptGoose}
	mgr := newForkNodeManager(t, be)
	fs := &fakeCPStream{}
	a := newAttacher(mgr, fs)

	a.handle(context.Background(), &nodev1.CPMessage{Msg: &nodev1.CPMessage_ForkSameNode{ForkSameNode: &nodev1.ForkSameNode{
		SourceSpawnId: "missing-source", ForkSpawnId: "sp-fork", SourceGeneration: 9, TargetGeneration: 1, TransferSetId: "ts-1",
	}}})

	waitFor(t, "ForkSameNodeComplete error", func() bool {
		fc := lastForkSameNodeComplete(fs)
		return fc != nil && fc.GetError() != ""
	})
	if hasPhase(fs.phasesFor("sp-fork"), nodev1.SpawnPhase_ACTIVE) {
		t.Fatal("failed fork materialization must not report fork ACTIVE")
	}
}
