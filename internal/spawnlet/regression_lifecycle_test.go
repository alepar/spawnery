package spawnlet

// regression_lifecycle_test.go: the SE1 §4.6 hermetic regression tests.
//
// WHICH LAYER THESE PIN — read this before trusting, extending, or citing this file.
//
// These tests drive the REAL Manager orchestration (internal/spawnlet/manager*.go) against the FAKE
// backend internal/runtime/fakepod. They therefore pin exactly ONE layer: the MANAGER'S SEQUENCING —
// who gets paused, what gets captured, when, from which state, and what happens on each failure arm.
//
// They do NOT — and BY CONSTRUCTION CANNOT — pin any behaviour inside a real backend
// (internal/runtime/docker*.go, internal/runtime/cri/). A fake-backed test never executes a line of
// that code. Backend behaviour is pinned by the PodBackend contract suite
// (internal/runtime/podbackendtest), whose lane arms run the same table against the REAL backends:
// Docker under `e2e`, CRI/runsc under `cri_delta_e2e` (`just test-cri-contract`). SE1 spec §4.4, §4.6.
//
// This distinction was got WRONG here once already, and the error is recorded rather than quietly
// edited away. The first draft of this header claimed R1 pinned "the CRI stop→capture→re-launch fork
// bug". It does not and cannot: that bug lives in internal/runtime/cri/delta.go, and reverting it
// leaves every test in THIS file green — the epic's final review did exactly that and watched all six
// pass. Its real pin is caseCaptureAsPreservesSource on the cri_delta_e2e contract arm (verified
// 2026-07-12: reverting `preserveSource` in cri/delta.go turns that case, and two others, red).
//
// The tests themselves are sound — only their descriptions were wrong. Reverting the Manager's deferred
// restoreSource() in manager_fork.go does turn R6a red. But a test that names a bug it cannot see buys
// false confidence, and false confidence is how that bug survived for weeks. If you add a test here,
// name the layer it pins.
//
//	R1 fork preserves its source  — Manager fork sequence: pause → CaptureDeltaAs → restore, same container.
//	R2 the fork's artifact carries the source's content as of the capture instant (Manager capture ordering).
//	R4 a captured delta is launchable and EnsureImage returns it (Manager's launch-image selection).
//	R5 resume replays the delta (Manager's resume path picks the delta, not the base).
//	R6 failure arms: capture fails → source restored; StartAgent fails → pod rolled back, not leaked.
//
// (R3, the torn-suspend regression, is SE2's — it cannot pass until SE2's fix (sp-2tx8.2.1) lands, since
// teardown still unconditionally unpauses before the rootfs capture. See sp-2tx8.1.5's bead notes.)

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

// R1 pins the MANAGER's fork sequence: ForkSameNode must pause the source, CaptureDeltaAs onto the
// target's tag, and restore the source to RUNNING — leaving the SAME source container alive (not
// re-launched) with its content untouched.
//
// It does NOT pin the CRI backend's fork, which once did stop → capture → remove and so destroyed the
// very spawn it forked from. That bug lives in internal/runtime/cri/delta.go; a fakepod-backed test
// never runs that code. Its pin is caseCaptureAsPreservesSource on the cri_delta_e2e contract arm.
// See the file header.
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

// R2 pins the MANAGER's capture ORDERING: the artifact must carry the source's content as of the
// capture instant, and nothing the source writes after the fork returns. Whether a real backend's diff
// is byte-faithful to the layer it claims to capture is a BACKEND property — pinned by
// caseCaptureArtifactInheritsContent on the contract lane arms, not here.
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

// R4 pins the MANAGER's launch-image SELECTION: after a suspend capture, the Manager asks EnsureImage
// for the delta and gets the delta; and when the delta is not launchable, it falls back to the base
// image rather than launching from a broken one.
//
// The shipped bugs in this class were BACKEND bugs — the assembled delta image was never Unpacked into
// the snapshotter, and it was recorded under a non-canonical ref while CRI normalises lookups. Neither
// is visible from here: fakepod models launchability as a single bit, so this test pins the Manager's
// REACTION to that bit, not a backend's ability to set it correctly. Those are pinned by
// caseEnsureImageLaunchableAfterCapture on the contract lane arms (e2e, cri_delta_e2e).
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

// R5 pins the MANAGER's resume path: a same-node resume Create must launch from the CAPTURED DELTA
// (LaunchImageRef = DeltaTag) and not silently fall back to the base image.
//
// That the resumed rootfs then carries the writes is fakepod replaying the launch image's content into
// the agent's rootfs — it makes "resumed from an empty base" a visible failure rather than a silent
// one, but it pins the Manager's image CHOICE, not any real snapshotter's replay fidelity. That stays
// with the contract lane arms.
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

// R6a pins the MANAGER's fork FAILURE ARM: when CaptureDeltaAs fails, ForkSameNode must error AND the
// source must come back to running — the deferred restore has to fire on the error path, not be skipped.
// A source left paused forever is a hung user spawn.
//
// This is the test that proves the hermetic layer is not vacuous: neutering the deferred restoreSource()
// in manager_fork.go turns it red. That is a Manager bug, and this is the layer that sees it.
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

// R6b pins the MANAGER's create ROLLBACK: when StartAgent fails, Create must tear the half-built pod
// down — sandbox and sidecar removed, nothing left in ListManaged, nothing left in the store. A leaked
// sandbox holds a netns, an IP and an egress floor forever.
//
// The rollback SEQUENCE is the Manager's, so it is pinned here. Whether a given backend's Remove
// actually reclaims the netns/IP is a backend property, and stays with the contract lane arms.
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
