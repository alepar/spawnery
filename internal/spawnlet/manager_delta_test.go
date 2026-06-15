package spawnlet

// manager_delta_test.go: hermetic tests for the rootfs delta-capture path
// (spec §2/§4, sp-ei4.1.10). Tests use fakePodBackend (defined in manager_sandbox_test.go)
// extended with the delta-capture control fields (capturedRef, captureErr, etc.).
//
// Test matrix:
//   E1: Suspend with DeltaCapture=true calls CaptureDelta and sets DeltaImageRef in-mem.
//   E2: Stop (not Suspend) does NOT call CaptureDelta.
//   E3: Suspend with DeltaCapture=false does NOT call CaptureDelta.
//   E4: Create launches from the delta tag when EnsureImage returns it.
//   E5: Create records BaseImageDigest from ResolveImageDigest.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/storage/journal"
)

// E1: Suspend with DeltaCapture=true triggers CaptureDelta before pod.Stop, and DeltaImageRef
// is set on the in-mem Spawn (before store.Delete removes it, we capture it via closures).
func TestSuspendCapturesDelta(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	sp, err := m.Create(ctx, "sp-delta", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spawnID := sp.ID

	if _, err := m.Suspend(ctx, spawnID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// CaptureDelta must have been called (fakePodBackend sets capturedRef on call).
	if fb.capturedRef == "" {
		t.Fatal("CaptureDelta was not called on Suspend with DeltaCapture=true")
	}
	// The captured ref must be the delta tag for the spawn id.
	wantRef := "spawnery/delta:" + spawnID
	if fb.capturedRef != wantRef {
		t.Fatalf("capturedRef = %q, want %q", fb.capturedRef, wantRef)
	}
	// The spawn must be removed from the store (teardown completed).
	if _, live := m.Store().Get(spawnID); live {
		t.Fatal("spawn must be removed from store after Suspend")
	}
}

// E2: Stop (not Suspend) must NOT call CaptureDelta — only Suspend triggers capture.
func TestStopDoesNotCapture(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	sp, err := m.Create(ctx, "sp-stop", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.Stop(ctx, sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if fb.capturedRef != "" {
		t.Fatalf("CaptureDelta must NOT be called on Stop; got capturedRef=%q", fb.capturedRef)
	}
}

// E3: Suspend with DeltaCapture=false must NOT call CaptureDelta (feature gate).
func TestCaptureGatedByConfig(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: false, // explicitly off
	})

	sp, err := m.Create(ctx, "sp-gate", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if fb.capturedRef != "" {
		t.Fatalf("CaptureDelta must NOT be called when DeltaCapture=false; got capturedRef=%q", fb.capturedRef)
	}
}

// E4: Create uses the image returned by EnsureImage (the delta tag) as the agent launch image.
func TestResumeLaunchesFromDelta(t *testing.T) {
	ctx := context.Background()
	const deltaRef = "spawnery/delta:sp-resume"

	fb := &fakePodBackend{ensureImageRef: deltaRef}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.Create(ctx, "sp-resume", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The agent spec captured by StartAgent must use the delta image.
	if fb.agentSpec.Image != deltaRef {
		t.Fatalf("agent launched from %q, want delta tag %q", fb.agentSpec.Image, deltaRef)
	}
}

// E4b: Without a delta image EnsureImage returns the base, and StartAgent uses the base.
func TestFreshCreateLaunchesFromBase(t *testing.T) {
	ctx := context.Background()
	const baseRef = "agent:base"

	fb := &fakePodBackend{} // ensureImageRef empty → returns baseRef
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: baseRef, SidecarImage: "s", DataRoot: t.TempDir(),
	})

	_, err := m.Create(ctx, "sp-fresh", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if fb.agentSpec.Image != baseRef {
		t.Fatalf("agent launched from %q, want base %q", fb.agentSpec.Image, baseRef)
	}
}

// E5: Create records the digest from ResolveImageDigest on the returned Spawn.
func TestCreateRecordsBaseImageDigest(t *testing.T) {
	ctx := context.Background()
	const digest = "img@sha256:cafebabe"

	fb := &fakePodBackend{resolveDigest: digest}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
	})

	sp, err := m.Create(ctx, "sp-digest", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if sp.BaseImageDigest != digest {
		t.Fatalf("BaseImageDigest = %q, want %q", sp.BaseImageDigest, digest)
	}
}

func TestMigrationSuspendStoresRootfsArtifactForSourceGeneration(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{resolveDigest: "agent@sha256:base"}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		NodeID: "node-a", AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, "sp-migrate-src", writeApp(t), "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := m.SuspendForMigration(ctx, sp.ID, true)
	if err != nil {
		t.Fatalf("SuspendForMigration: %v", err)
	}
	if len(result.RootfsArtifacts) != 1 {
		t.Fatalf("RootfsArtifacts = %+v, want one artifact", result.RootfsArtifacts)
	}
	if len(fj.artifactPuts) != 1 {
		t.Fatalf("journal PutArtifact calls = %d, want 1", len(fj.artifactPuts))
	}
	got := fj.artifactPuts[0]
	if got.SpawnID != sp.ID || got.Generation != 7 {
		t.Fatalf("artifact key = %s/%d, want %s/7", got.SpawnID, got.Generation, sp.ID)
	}
	if got.Type != journal.ArtifactRootfsDelta || got.Format != journal.ArtifactFormatOCILayout {
		t.Fatalf("artifact type/format = %s/%s", got.Type, got.Format)
	}
	if got.BaseImageDigest != "agent@sha256:base" {
		t.Fatalf("artifact base digest = %q", got.BaseImageDigest)
	}
	if got.ProducerNodeID != "node-a" || got.ProducerRuntime == "" {
		t.Fatalf("producer metadata = node %q runtime %q", got.ProducerNodeID, got.ProducerRuntime)
	}
	if string(fj.artifactPayloads[got.ArtifactID]) != runtime.DeltaTag(sp.ID) {
		t.Fatalf("artifact payload = %q, want exported delta tag", fj.artifactPayloads[got.ArtifactID])
	}
	if !strings.Contains(strings.Join(fb.ops, ","), "export:"+sp.ID) {
		t.Fatalf("expected ExportDelta before migration suspend completes, ops=%v", fb.ops)
	}
}

func TestMigrationSuspendIncludesInheritedRootfsArtifactChain(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-migrate-chain"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{resolveDigest: base}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		NodeID: "node-a", AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, spawnID, writeApp(t), "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.RootfsArtifacts = []RootfsArtifact{{
		ArtifactID: "inherited-rootfs-gen7-seq1", Generation: 7, Sequence: 1,
		BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout,
	}}
	sp.DeltaDepth = 1

	result, err := m.SuspendForMigration(ctx, sp.ID, true)
	if err != nil {
		t.Fatalf("SuspendForMigration: %v", err)
	}
	if len(result.RootfsArtifacts) != 2 {
		t.Fatalf("RootfsArtifacts = %+v, want inherited chain plus captured top layer", result.RootfsArtifacts)
	}
	if got := result.RootfsArtifacts[0]; got.ArtifactID != "inherited-rootfs-gen7-seq1" || got.Generation != 7 || got.Sequence != 1 {
		t.Fatalf("inherited rootfs artifact = %+v", got)
	}
	if got := result.RootfsArtifacts[1]; got.ArtifactID == "" || got.ArtifactID == "inherited-rootfs-gen7-seq1" ||
		got.Generation != 7 || got.Sequence != 2 || got.BaseImageDigest != base {
		t.Fatalf("captured top rootfs artifact = %+v", got)
	}
}

func TestMigrationSuspendRekeysInheritedRootfsArtifactChainToCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-migrate-chain-again"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{resolveDigest: base}
	fj := newFakeJournal("")
	fj.artifactPayloads["inherited-rootfs-gen4-seq1"] = []byte("inherited-layer")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		NodeID: "node-a", AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, spawnID, writeApp(t), "model", "", "", 5)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.RootfsArtifacts = []RootfsArtifact{{
		ArtifactID: "inherited-rootfs-gen4-seq1", Generation: 4, Sequence: 1,
		BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout,
	}}
	sp.DeltaDepth = 1

	result, err := m.SuspendForMigration(ctx, sp.ID, true)
	if err != nil {
		t.Fatalf("SuspendForMigration: %v", err)
	}
	if len(result.RootfsArtifacts) != 2 {
		t.Fatalf("RootfsArtifacts = %+v, want re-keyed inherited chain plus captured top layer", result.RootfsArtifacts)
	}
	if got := result.RootfsArtifacts[0]; got.ArtifactID != "inherited-rootfs-gen4-seq1" ||
		got.Generation != 5 || got.Sequence != 1 || got.BaseImageDigest != base || got.Format != journal.ArtifactFormatOCILayout {
		t.Fatalf("re-keyed inherited rootfs artifact = %+v", got)
	}
	if got := result.RootfsArtifacts[1]; got.Generation != 5 || got.Sequence != 2 || got.BaseImageDigest != base {
		t.Fatalf("captured top rootfs artifact = %+v", got)
	}
	if len(fj.artifactGets) != 1 || fj.artifactGets[0].Generation != 4 || fj.artifactGets[0].ArtifactID != "inherited-rootfs-gen4-seq1" {
		t.Fatalf("inherited artifact gets = %+v, want source generation 4", fj.artifactGets)
	}
	if len(fj.artifactPuts) != 2 || fj.artifactPuts[0].Generation != 5 || fj.artifactPuts[0].ArtifactID != "inherited-rootfs-gen4-seq1" ||
		fj.artifactPuts[1].Generation != 5 || fj.artifactPuts[1].Sequence != 2 {
		t.Fatalf("artifact puts = %+v, want re-keyed inherited then captured top layer", fj.artifactPuts)
	}
}

func TestMigrationSuspendFailsClosedWithUnexportedLocalRootfsHistory(t *testing.T) {
	ctx := context.Background()
	const spawnID = "sp-unportable-local-chain"
	fb := &fakePodBackend{resolveDigest: "agent@sha256:base"}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		NodeID: "node-a", AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, spawnID, writeApp(t), "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.DeltaDepth = 2

	_, err = m.SuspendForMigration(ctx, sp.ID, true)
	if err == nil || !strings.Contains(err.Error(), "unexported local rootfs delta history") {
		t.Fatalf("SuspendForMigration error = %v, want unexported local rootfs delta history", err)
	}
	if len(fj.artifactPuts) != 0 {
		t.Fatalf("must not upload rootfs artifacts for unportable local chain, puts=%+v", fj.artifactPuts)
	}
}

func TestNormalSuspendDoesNotStoreRootfsArtifact(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.Create(ctx, "sp-local-suspend", writeApp(t), "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(fj.artifactPuts) != 0 {
		t.Fatalf("normal same-node Suspend must not upload rootfs artifacts, got %+v", fj.artifactPuts)
	}
}

func TestCreateRestoresPinnedRootfsArtifactBeforeLaunch(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID    = "sp-migrate-target"
		base       = "agent@sha256:base"
		artifactID = "artifact-rootfs-gen4"
	)
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	fj.artifactPayloads[artifactID] = []byte(runtime.DeltaTag(spawnID))
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, spawnID, writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        base,
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{{
			ArtifactID: artifactID, Generation: 4, Sequence: 1, BaseImageDigest: base,
			Format: journal.ArtifactFormatOCILayout,
		}},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}
	if len(fj.artifactGets) != 1 {
		t.Fatalf("journal GetArtifact calls = %d, want 1", len(fj.artifactGets))
	}
	got := fj.artifactGets[0]
	if got.SpawnID != spawnID || got.Generation != 4 || got.ArtifactID != artifactID {
		t.Fatalf("GetArtifact key = %+v", got)
	}
	if fb.agentSpec.Image != runtime.DeltaTag(spawnID) {
		t.Fatalf("agent launched from %q, want imported delta tag", fb.agentSpec.Image)
	}
	if !strings.Contains(strings.Join(fb.ops, ","), "import:"+spawnID) {
		t.Fatalf("expected ImportDelta before launch, ops=%v", fb.ops)
	}
}

func TestCreateRestoresRootfsArtifactChainSequentially(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-chain-target"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	fj.artifactPayloads["artifact-rootfs-seq1"] = []byte("layer-1")
	fj.artifactPayloads["artifact-rootfs-seq2"] = []byte("layer-2")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, spawnID, writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        base,
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{
			{ArtifactID: "artifact-rootfs-seq1", Generation: 4, Sequence: 1, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
			{ArtifactID: "artifact-rootfs-seq2", Generation: 4, Sequence: 2, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
		},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}
	want := []string{base, runtime.DeltaTag(spawnID)}
	if !reflect.DeepEqual(fb.importBaseRefs, want) {
		t.Fatalf("rootfs artifact import bases = %v, want sequential chain %v", fb.importBaseRefs, want)
	}
	if fb.agentSpec.Image != runtime.DeltaTag(spawnID) {
		t.Fatalf("agent launched from %q, want imported delta tag", fb.agentSpec.Image)
	}
}

func TestCreateRestoresRootfsArtifactsSortedBySequenceAndDepth(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-chain-depth"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	fj.artifactPayloads["artifact-rootfs-seq1"] = []byte("layer-1")
	fj.artifactPayloads["artifact-rootfs-seq3"] = []byte("layer-3")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	sp, err := m.CreateWithSelection(ctx, spawnID, writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        base,
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{
			{ArtifactID: "artifact-rootfs-seq3", Generation: 4, Sequence: 2, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
			{ArtifactID: "artifact-rootfs-seq1", Generation: 4, Sequence: 1, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
		},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}
	if sp.DeltaDepth != 2 {
		t.Fatalf("DeltaDepth = %d, want max restored rootfs sequence 2", sp.DeltaDepth)
	}
	if len(fj.artifactGets) != 2 ||
		fj.artifactGets[0].ArtifactID != "artifact-rootfs-seq1" ||
		fj.artifactGets[1].ArtifactID != "artifact-rootfs-seq3" {
		t.Fatalf("rootfs artifact restore order = %+v, want sequence order", fj.artifactGets)
	}
}

func TestCreateRejectsRootfsArtifactSequenceGapsAndDuplicates(t *testing.T) {
	ctx := context.Background()
	const base = "agent@sha256:base"
	for _, tc := range []struct {
		name      string
		artifacts []RootfsArtifact
		want      string
	}{
		{
			name: "gap",
			artifacts: []RootfsArtifact{
				{ArtifactID: "artifact-rootfs-seq1", Generation: 4, Sequence: 1, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
				{ArtifactID: "artifact-rootfs-seq3", Generation: 4, Sequence: 3, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
			},
			want: "sequence gap",
		},
		{
			name: "duplicate",
			artifacts: []RootfsArtifact{
				{ArtifactID: "artifact-rootfs-a", Generation: 4, Sequence: 1, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
				{ArtifactID: "artifact-rootfs-b", Generation: 4, Sequence: 1, BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout},
			},
			want: "duplicate sequence",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakePodBackend{}
			fj := newFakeJournal("")
			for _, art := range tc.artifacts {
				fj.artifactPayloads[art.ArtifactID] = []byte("layer")
			}
			m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
				AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
				DeltaCapture: true,
			})
			m.SetJournal(fj, t.TempDir())

			_, err := m.CreateWithSelection(ctx, "sp-bad-"+tc.name, writeApp(t), "model", "", "", 5, AgentSelection{
				BaseImageDigest:        base,
				RootfsSourceGeneration: 4,
				RootfsArtifacts:        tc.artifacts,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateWithSelection error = %v, want %q", err, tc.want)
			}
			if len(fj.artifactGets) != 0 {
				t.Fatalf("must not restore malformed rootfs chain, gets=%+v", fj.artifactGets)
			}
		})
	}
}

func TestCreateRestoresRootfsArtifactsEvenWhenLocalDeltaTagExists(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-stale-local-delta"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{ensureImageRef: runtime.DeltaTag(spawnID)}
	fj := newFakeJournal("")
	fj.artifactPayloads["artifact-rootfs-seq1"] = []byte("restored-layer")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, spawnID, writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        base,
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{{
			ArtifactID: "artifact-rootfs-seq1", Generation: 4, Sequence: 1,
			BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout,
		}},
	})
	if err != nil {
		t.Fatalf("CreateWithSelection: %v", err)
	}
	if len(fb.importBaseRefs) != 1 || fb.importBaseRefs[0] != base {
		t.Fatalf("ImportDelta bases = %v, want [%s]", fb.importBaseRefs, base)
	}
	if fb.agentSpec.Image != runtime.DeltaTag(spawnID) {
		t.Fatalf("agent launched from %q, want imported delta tag", fb.agentSpec.Image)
	}
}

func TestCreateRootfsLocalOnlyRequiresLocalDeltaTag(t *testing.T) {
	ctx := context.Background()
	const (
		spawnID = "sp-missing-local-delta"
		base    = "agent@sha256:base"
	)
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, spawnID, writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:          base,
		RootfsSourceGeneration:   4,
		RootfsArtifactsLocalOnly: true,
		RootfsArtifacts: []RootfsArtifact{{
			ArtifactID: "artifact-rootfs-seq1", Generation: 4, Sequence: 1,
			BaseImageDigest: base, Format: journal.ArtifactFormatOCILayout,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "missing local delta image") {
		t.Fatalf("CreateWithSelection error = %v, want missing local delta image", err)
	}
	if len(fb.importBaseRefs) != 0 {
		t.Fatalf("local-only start must not import rootfs artifacts, import bases=%v", fb.importBaseRefs)
	}
}

func TestCreateRejectsRootfsArtifactBaseMismatch(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, "sp-bad-artifact", writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        "agent@sha256:base",
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{{
			ArtifactID: "artifact-rootfs-gen4", Generation: 4, Sequence: 1, BaseImageDigest: "agent@sha256:other",
			Format: journal.ArtifactFormatOCILayout,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "base digest") {
		t.Fatalf("CreateWithSelection must reject rootfs artifact base mismatch, got %v", err)
	}
}

func TestCreateRejectsUnpinnedRootfsArtifact(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	fj := newFakeJournal("")
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	m.SetJournal(fj, t.TempDir())

	_, err := m.CreateWithSelection(ctx, "sp-unpinned-artifact", writeApp(t), "model", "", "", 5, AgentSelection{
		BaseImageDigest:        "agent@sha256:base",
		RootfsSourceGeneration: 4,
		RootfsArtifacts: []RootfsArtifact{{
			Generation: 4, Sequence: 1, BaseImageDigest: "agent@sha256:base",
			Format: journal.ArtifactFormatOCILayout,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "empty artifact id") {
		t.Fatalf("CreateWithSelection must reject unpinned rootfs artifact, got %v", err)
	}
	if len(fj.artifactGets) != 0 {
		t.Fatalf("journal must not be queried for unpinned artifact, got %+v", fj.artifactGets)
	}
}
