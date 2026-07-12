package spawnlet

// regression_lifecycle_test.go: the SE1 §4.6 regression tests. Each one pins a bug that shipped to
// master and was found by a human running a VM, not by a test:
//
//	R1 fork preserves its source          — the CRI stop→capture→re-launch fork destroyed it.
//	R2 the fork's artifact inherits the source's content at capture time.
//	R4 a captured delta is LAUNCHABLE     — the delta image was never unpacked / recorded non-canonically.
//	R5 resume replays the delta.
//	R6 failure arms: capture fails → source restored; StartAgent fails → pod rolled back, not leaked.
//
// (R3, the torn-suspend regression, is SE2's — it cannot pass until SE2's fix (sp-2tx8.2.1) lands, since
// teardown still unconditionally unpauses before the rootfs capture. See sp-2tx8.1.5's bead notes.)
//
// All of them drive the REAL Manager orchestration against internal/runtime/fakepod, so they run in
// milliseconds with no Docker. What they do NOT cover: durability, containerd ref normalisation,
// snapshotter unpacking, gVisor quirks — those stay with the e2e/VM lanes (SE1 spec §4.4).

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

// newRegressionManager builds a hermetic Manager on a fakepod backend with delta capture on, the
// fake's exec seam wired as the scrub fn, and (optionally) a journaler — ForkSameNode requires one.
func newRegressionManager(t *testing.T, j *fakeJournal, opts ...fakepod.Option) (*Manager, *fakepod.Backend) {
	t.Helper()
	b := fakepod.New(opts...)
	t.Cleanup(b.Close)
	m := NewManagerWithBackend(b, &fakeApplier{}, ManagerConfig{
		NodeID: "node-1", AgentImage: "agent:base", SidecarImage: "sidecar:base",
		DataRoot: t.TempDir(), DeltaCapture: true,
	})
	// The default scrubFn shells out to `docker exec`; point it at the fake's exec seam instead so
	// the scrub is hermetic AND observable (it is what makes the paused-exec failure real).
	m.scrubFn = b.ScrubFn()
	if j != nil {
		m.SetJournal(j, t.TempDir())
	}
	return m, b
}

// R1: a fork must NOT destroy its source. After ForkSameNode the source agent is RUNNING (restored
// from the fork pause), it is the SAME container (not re-launched), and its content is untouched.
func TestRegressionForkPreservesSource(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, newFakeJournal("manifest-1"))

	sp, err := m.Create(ctx, "sp-source", writeJournalApp(t), "model", "", "", 7)
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	agentBefore := sp.AgentID
	if err := b.AgentWrite("sp-source", "/work/notes.txt", []byte("v1")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}

	if _, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID:    "sp-source",
		ForkSpawnID:      "sp-fork",
		SourceGeneration: 7,
		TargetGeneration: 1,
	}); err != nil {
		t.Fatalf("ForkSameNode: %v", err)
	}

	if got := b.State("sp-source", "agent"); got != fakepod.StateRunning {
		t.Fatalf("source agent is %s after fork, want %s (the fork destroyed its own source)",
			got, fakepod.StateRunning)
	}
	live, ok := m.Store().Get("sp-source")
	if !ok {
		t.Fatal("source spawn dropped from the store by a fork")
	}
	if live.AgentID != agentBefore {
		t.Fatalf("source agent id changed %q -> %q: the source was re-launched, not preserved",
			agentBefore, live.AgentID)
	}
	if got := string(b.RootfsView("sp-source")["/work/notes.txt"]); got != "v1" {
		t.Fatalf("source rootfs /work/notes.txt = %q, want %q", got, "v1")
	}
}

// R2: the fork's artifact inherits the SOURCE's content as of the capture instant — and nothing the
// source writes afterwards.
func TestRegressionForkArtifactInheritsSourceContent(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, newFakeJournal("manifest-1"))

	if _, err := m.Create(ctx, "sp-source", writeJournalApp(t), "model", "", "", 7); err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := b.AgentWrite("sp-source", "/work/notes.txt", []byte("before-fork")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}

	if _, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID: "sp-source", ForkSpawnID: "sp-fork",
		SourceGeneration: 7, TargetGeneration: 1,
	}); err != nil {
		t.Fatalf("ForkSameNode: %v", err)
	}
	// The source keeps running and keeps writing — this must NOT reach the fork's artifact.
	if err := b.AgentWrite("sp-source", "/work/notes.txt", []byte("after-fork")); err != nil {
		t.Fatalf("AgentWrite after fork: %v", err)
	}

	content, ok := b.ImageContent(runtime.DeltaTag("sp-fork"))
	if !ok {
		t.Fatalf("no image %s: the fork captured nothing", runtime.DeltaTag("sp-fork"))
	}
	if got := string(content["/work/notes.txt"]); got != "before-fork" {
		t.Fatalf("fork artifact /work/notes.txt = %q, want %q (capture-instant content)", got, "before-fork")
	}
}

// R4: after a suspend capture, the delta image exists, is LAUNCHABLE, and EnsureImage returns it.
// The shipped bugs in this class: the assembled delta image was never Unpacked into the snapshotter
// (so CRI could not launch it), and it was recorded under a non-canonical ref. The fake models the
// launchability half; the ref/snapshotter half stays with the CRI e2e lane (SE1 §4.4).
func TestRegressionCapturedDeltaIsLaunchable(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, nil)

	if _, err := m.Create(ctx, "sp-cap", writeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.AgentWrite("sp-cap", "/work/notes.txt", []byte("captured")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	if _, err := m.Suspend(ctx, "sp-cap"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	deltaRef := runtime.DeltaTag("sp-cap")
	img, ok := b.Images()[deltaRef]
	if !ok {
		t.Fatalf("no delta image %s after suspend", deltaRef)
	}
	if !img.Launchable {
		t.Fatalf("delta image %s is not launchable (committed but never made runnable)", deltaRef)
	}
	got, err := m.pod.EnsureImage(ctx, "agent:base", deltaRef)
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if got != deltaRef {
		t.Fatalf("EnsureImage = %q, want the delta %q", got, deltaRef)
	}

	// Negative arm: a delta that exists but cannot be launched must fall back to the base — and the
	// next Create must then launch from the base, NOT from a broken delta.
	b.MarkUnlaunchable(deltaRef)
	got, err = m.pod.EnsureImage(ctx, "agent:base", deltaRef)
	if err != nil {
		t.Fatalf("EnsureImage (unlaunchable): %v", err)
	}
	if got != "agent:base" {
		t.Fatalf("EnsureImage of an unlaunchable delta = %q, want the base %q", got, "agent:base")
	}
}

// R5: a same-node resume launches from the captured delta and the resumed agent SEES the writes.
// (fakepod's StartAgent replays the launch image's content into the agent's rootfs — so "the spawn
// resumed from an empty base image" is a visible failure, not a silent one.)
func TestRegressionResumeReplaysDelta(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, nil)

	if _, err := m.Create(ctx, "sp-res", writeApp(t), "model", "", "", 1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.AgentWrite("sp-res", "/work/notes.txt", []byte("R1")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	if _, err := m.Suspend(ctx, "sp-res"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp, err := m.Create(ctx, "sp-res", writeApp(t), "model", "", "", 2) // same id = same-node resume
	if err != nil {
		t.Fatalf("resume Create: %v", err)
	}
	if want := runtime.DeltaTag("sp-res"); sp.LaunchImageRef != want {
		t.Fatalf("LaunchImageRef = %q, want the delta %q (resume fell back to the base image)",
			sp.LaunchImageRef, want)
	}
	if got := string(b.RootfsView("sp-res")["/work/notes.txt"]); got != "R1" {
		t.Fatalf("resumed rootfs /work/notes.txt = %q, want %q (the delta was not replayed)", got, "R1")
	}
	if got := b.State("sp-res", "agent"); got != fakepod.StateRunning {
		t.Fatalf("resumed agent is %s, want %s", got, fakepod.StateRunning)
	}
}

// R6a: failure arm — when the fork's CaptureDeltaAs fails, the fork errors AND the source is restored
// to running (the deferred restore must fire, not be skipped on the error path). A source left paused
// forever is a hung user spawn.
func TestRegressionForkCaptureFailureRestoresSource(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, newFakeJournal("manifest-1"))

	if _, err := m.Create(ctx, "sp-source", writeJournalApp(t), "model", "", "", 7); err != nil {
		t.Fatalf("Create source: %v", err)
	}
	b.FailOn(fakepod.OpCaptureDeltaAs, errors.New("boom"))

	if _, err := m.ForkSameNode(ctx, ForkSameNodeRequest{
		SourceSpawnID: "sp-source", ForkSpawnID: "sp-fork",
		SourceGeneration: 7, TargetGeneration: 1,
	}); err == nil {
		t.Fatal("ForkSameNode must fail when the capture fails")
	}

	if got := b.State("sp-source", "agent"); got != fakepod.StateRunning {
		t.Fatalf("source agent is %s after a failed fork, want %s (source left paused/dead)",
			got, fakepod.StateRunning)
	}
	if _, ok := m.Store().Get("sp-source"); !ok {
		t.Fatal("source spawn dropped from the store by a failed fork")
	}
	// The source must be restored via RestoreForkedSource, not left to a stray Unpause.
	var restored bool
	for _, op := range b.Ops() {
		if op == string(fakepod.OpRestoreForked)+":sp-source" {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("ops = %v, want a %s:sp-source", b.Ops(), fakepod.OpRestoreForked)
	}
}

// R6b: failure arm — when StartAgent fails, Create must tear the half-built pod down (sandbox +
// sidecar removed, nothing left in ListManaged, nothing in the store). A leaked sandbox holds a netns,
// an IP and an egress floor forever.
func TestRegressionStartAgentFailureRollsBackPod(t *testing.T) {
	ctx := context.Background()
	m, b := newRegressionManager(t, nil, fakepod.WithFailOn(fakepod.OpStartAgent, errors.New("boom")))

	if _, err := m.Create(ctx, "sp-rb", writeApp(t), "model", "", "", 1); err == nil {
		t.Fatal("Create must fail when StartAgent fails")
	}

	if got := b.State("sp-rb", "sandbox"); got != fakepod.StateRemoved {
		t.Fatalf("sandbox is %s after a failed StartAgent, want %s (leaked pod)", got, fakepod.StateRemoved)
	}
	if got := b.State("sp-rb", "sidecar"); got != fakepod.StateRemoved {
		t.Fatalf("sidecar is %s after a failed StartAgent, want %s (leaked pod)", got, fakepod.StateRemoved)
	}
	managed, err := m.pod.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	for _, mp := range managed {
		if mp.SpawnID == "sp-rb" {
			t.Fatalf("pod %s still managed after a failed StartAgent: %+v", mp.SpawnID, mp)
		}
	}
	if _, ok := m.Store().Get("sp-rb"); ok {
		t.Fatal("spawn sp-rb is in the store after a failed Create")
	}
}
