package podbackendtest

import (
	"bytes"
	"context"
	"testing"

	"spawnery/internal/runtime"
)

// releaseDelta registers the delta tag for GC at test end.
func releaseDelta(ctx context.Context, t *testing.T, e *Env, spawnID string) {
	t.Helper()
	t.Cleanup(func() { _ = e.Backend.ReleaseDelta(context.WithoutCancel(ctx), spawnID) })
}

// caseCaptureAsPreservesSource is THE fork regression. CaptureDeltaAs commits the SOURCE agent's
// writable layer to the TARGET spawn's delta tag; it must leave the source alive and unremoved. The
// shipped CRI lane used to stop -> capture -> re-launch, i.e. a fork destroyed the spawn it forked
// from. Nothing in the old fakes could express that, which is why it shipped.
//
// It drives the real fork sequence (pause the source, capture as the target, restore the source) and
// then proves the source is alive three ways: the backend still lists it, Attach still works, and —
// the sharpest — it still takes a write, which a stopped or removed container cannot.
func caseCaptureAsPreservesSource(t *testing.T, e *Env) {
	ctx := t.Context()
	src := uniqueSpawnID(t)
	fork := uniqueSpawnID(t)

	h := startPod(ctx, t, e, src, e.BaseImage, 1)
	if err := e.Write(ctx, h, e.RootfsFile, []byte("v1")); err != nil {
		t.Fatalf("write to the source agent: %v", err)
	}

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause the source: %v", err)
	}
	releaseDelta(ctx, t, e, fork)
	ref, err := e.Backend.CaptureDeltaAs(ctx, h, fork)
	if err != nil {
		t.Fatalf("CaptureDeltaAs(%s -> %s): %v", src, fork, err)
	}
	if want := runtime.DeltaTag(fork); ref != want {
		t.Fatalf("CaptureDeltaAs ref = %q, want %q (the ref MUST be the canonical tag — the CRI lane once "+
			"recorded a non-canonical one and then could not find its own image)", ref, want)
	}
	if err := e.Backend.RestoreForkedSource(ctx, h); err != nil {
		t.Fatalf("RestoreForkedSource: %v", err)
	}

	pods, err := e.Backend.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if _, ok := findManaged(pods, src); !ok {
		t.Fatalf("after CaptureDeltaAs the source pod %s is gone from ListManaged: %+v — the fork REMOVED its own source", src, pods)
	}
	s, err := e.Backend.Attach(ctx, h)
	if err != nil {
		t.Fatalf("after CaptureDeltaAs the source agent is not attachable: %v — the fork stopped its own source", err)
	}
	closeStream(s)
	if err := e.Write(ctx, h, e.RootfsFile, []byte("v2")); err != nil {
		t.Fatalf("after CaptureDeltaAs the source agent no longer takes writes: %v — the fork stopped or removed its own source", err)
	}
}

// caseCaptureArtifactInheritsContent: the captured artifact must carry the source's writable-layer
// content as of the capture. An empty artifact that "succeeds" is the worst failure mode there is —
// the fork comes up, and it is blank.
func caseCaptureArtifactInheritsContent(t *testing.T, e *Env) {
	ctx := t.Context()
	src := uniqueSpawnID(t)
	fork := uniqueSpawnID(t)
	want := []byte("hello-from-the-source")

	h := startPod(ctx, t, e, src, e.BaseImage, 1)
	if err := e.Write(ctx, h, e.RootfsFile, want); err != nil {
		t.Fatalf("write to the source agent: %v", err)
	}

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause the source: %v", err)
	}
	releaseDelta(ctx, t, e, fork)
	ref, err := e.Backend.CaptureDeltaAs(ctx, h, fork)
	if err != nil {
		t.Fatalf("CaptureDeltaAs(%s -> %s): %v", src, fork, err)
	}
	if err := e.Backend.RestoreForkedSource(ctx, h); err != nil {
		t.Fatalf("RestoreForkedSource: %v", err)
	}

	got, err := e.ReadArtifact(ctx, ref, e.RootfsFile)
	if err != nil {
		t.Fatalf("read %s out of the captured delta %s: %v", e.RootfsFile, ref, err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), want) {
		t.Fatalf("captured delta %s holds %q at %s, want %q — the artifact did not inherit the source's rootfs",
			ref, got, e.RootfsFile, want)
	}
}

// caseEnsureImageLaunchableAfterCapture is the image-visibility class: the CRI lane once committed a
// delta that was never Unpacked into the snapshotter, and recorded it under a ref the runtime then
// normalised differently — so EnsureImage handed back a ref that could not be launched. "Exists" is
// not the bar; LAUNCHES is.
func caseEnsureImageLaunchableAfterCapture(t *testing.T, e *Env) {
	ctx := t.Context()
	src := uniqueSpawnID(t)
	fork := uniqueSpawnID(t)
	deltaTag := runtime.DeltaTag(fork)

	h := startPod(ctx, t, e, src, e.BaseImage, 1)
	if err := e.Write(ctx, h, e.RootfsFile, []byte("seed")); err != nil {
		t.Fatalf("write to the source agent: %v", err)
	}

	// Before any capture there is no delta, so EnsureImage falls back to the base — that is what a
	// fresh create depends on.
	got, err := e.Backend.EnsureImage(ctx, e.BaseImage, deltaTag)
	if err != nil {
		t.Fatalf("EnsureImage before any capture: %v", err)
	}
	if got != e.BaseImage {
		t.Fatalf("EnsureImage before any capture = %q, want the base %q", got, e.BaseImage)
	}

	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause the source: %v", err)
	}
	releaseDelta(ctx, t, e, fork)
	if _, err := e.Backend.CaptureDeltaAs(ctx, h, fork); err != nil {
		t.Fatalf("CaptureDeltaAs(%s -> %s): %v", src, fork, err)
	}
	if err := e.Backend.RestoreForkedSource(ctx, h); err != nil {
		t.Fatalf("RestoreForkedSource: %v", err)
	}

	got, err = e.Backend.EnsureImage(ctx, e.BaseImage, deltaTag)
	if err != nil {
		t.Fatalf("EnsureImage after capture: %v", err)
	}
	if got != deltaTag {
		t.Fatalf("EnsureImage after capture = %q, want the delta %q — the captured image is not visible to "+
			"the runtime (never unpacked, or recorded under a ref the runtime does not resolve)", got, deltaTag)
	}

	// LAUNCHABLE, not merely present: start a real agent from the ref EnsureImage handed back. This is
	// the assertion that would have caught the un-unpacked delta image; an existence check would not.
	h2 := startPod(ctx, t, e, fork, got, 1)
	s, err := e.Backend.Attach(ctx, h2)
	if err != nil {
		t.Fatalf("an agent launched from the captured delta %s is not attachable: %v", got, err)
	}
	closeStream(s)
}

// caseCaptureDeltaOnPausedAgent is SE2's backend-side pin (sp-2tx8.2.1). The suspend gate PAUSES the agent
// to snapshot the journaled mounts and never unpauses it, so a self-capture must work on a FROZEN agent and
// the artifact must carry exactly the content the agent had at the pause instant. A lane that quietly
// resumes the container to capture it (the CRI lane used to) reopens the window between the two halves of
// the suspend artifact — a torn snapshot, invisible from the Manager.
func caseCaptureDeltaOnPausedAgent(t *testing.T, e *Env) {
	ctx := t.Context()
	id := uniqueSpawnID(t)
	want := []byte("frozen-at-the-pause")

	h := startPod(ctx, t, e, id, e.BaseImage, 1)
	if err := e.Write(ctx, h, e.RootfsFile, want); err != nil {
		t.Fatalf("write to the agent: %v", err)
	}
	if err := e.Backend.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	releaseDelta(ctx, t, e, id)
	ref, err := e.Backend.CaptureDelta(ctx, h)
	if err != nil {
		t.Fatalf("CaptureDelta on a PAUSED agent: %v — the suspend gate never releases the pause, so this "+
			"is the ONLY state a suspend capture ever sees", err)
	}
	if wantRef := runtime.DeltaTag(id); ref != wantRef {
		t.Fatalf("CaptureDelta ref = %q, want %q", ref, wantRef)
	}

	got, err := e.ReadArtifact(ctx, ref, e.RootfsFile)
	if err != nil {
		t.Fatalf("read %s out of the captured delta %s: %v", e.RootfsFile, ref, err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), want) {
		t.Fatalf("delta %s captured from the paused agent holds %q at %s, want %q",
			ref, got, e.RootfsFile, want)
	}
}

// caseCaptureLayerCountGuard: a commit that adds no layer (moby#47065) yields a delta image that
// silently drops the agent's writes. The backend must refuse it rather than hand back an image that
// looks fine and is empty.
func caseCaptureLayerCountGuard(t *testing.T, e *Env) {
	ctx := t.Context()
	src := uniqueSpawnID(t)
	target := uniqueSpawnID(t)

	h := startPod(ctx, t, e, src, e.BaseImage, 1)

	disarm := e.ArmZeroLayerCapture()
	t.Cleanup(disarm)

	releaseDelta(ctx, t, e, target)
	ref, err := e.Backend.CaptureDeltaAs(ctx, h, target)
	if err == nil {
		t.Fatalf("CaptureDeltaAs with a zero-layer commit returned %q and no error — the moby#47065 guard "+
			"did not fire, so a delta image with none of the agent's writes would be handed to resume", ref)
	}
}
