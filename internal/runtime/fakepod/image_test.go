package fakepod_test

import (
	"bytes"
	"context"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

// captureHandle is startPod's handle with the two fields the Manager sets before a capture.
func captureHandle(t *testing.T, b *fakepod.Backend, id string) *runtime.PodHandle {
	t.Helper()
	h := startPod(t, b, id)
	h.SpawnID = id
	h.BaseImageRef = "agent:base"
	return h
}

func TestCaptureDeltaAsPreservesTheSource(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if err := b.AgentWrite("sp1", "/work/a", []byte("A")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}

	ref, err := b.CaptureDeltaAs(ctx, h, "sp2")
	if err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	if ref != runtime.DeltaTag("sp2") {
		t.Fatalf("ref = %q, want %q", ref, runtime.DeltaTag("sp2"))
	}
	// THE fork bug: the source must still be running, with its content untouched.
	if got := b.State("sp1", "agent"); got != fakepod.StateRunning {
		t.Fatalf("source agent = %s after CaptureDeltaAs, want running", got)
	}
	if string(b.RootfsView("sp1")["/work/a"]) != "A" {
		t.Fatal("CaptureDeltaAs lost the source's content")
	}
	// The artifact inherits the source's content at capture time.
	content, ok := b.ImageContent(ref)
	if !ok || string(content["/work/a"]) != "A" {
		t.Fatalf("delta content = %v, want /work/a=A", content)
	}
}

func TestCaptureSnapshotsAtCallTime(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if err := b.AgentWrite("sp1", "/work/a", []byte("before")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	ref, err := b.CaptureDeltaAs(ctx, h, "sp1")
	if err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	if err := b.AgentWrite("sp1", "/work/a", []byte("after")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	content, _ := b.ImageContent(ref)
	if string(content["/work/a"]) != "before" {
		t.Fatalf("artifact mutated after capture: %q, want %q", content["/work/a"], "before")
	}
}

func TestCaptureDeltaStopsTheAgentAndRefusesARemovedOne(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if _, err := b.CaptureDelta(ctx, h); err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}
	// CaptureDelta stops+commits (PodBackend doc); Stop then removes.
	if got := b.State("sp1", "agent"); got != fakepod.StateStopped {
		t.Fatalf("agent = %s after CaptureDelta, want stopped", got)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := b.CaptureDelta(ctx, h); err == nil {
		t.Fatal("CaptureDelta on a removed agent must fail, got nil")
	}
}

func TestZeroLayerGuard(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3), fakepod.WithZeroLayerCapture())
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if _, err := b.CaptureDelta(ctx, h); err == nil {
		t.Fatal("a commit with layers <= base must trip the moby#47065 guard, got nil")
	}
}

func TestEnsureImageReturnsALaunchableDelta(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")

	// Before any capture there is no delta: fall back to the base.
	got, err := b.EnsureImage(ctx, "agent:base", runtime.DeltaTag("sp1"))
	if err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if got != "agent:base" {
		t.Fatalf("EnsureImage before capture = %q, want agent:base", got)
	}

	ref, err := b.CaptureDeltaAs(ctx, h, "sp1")
	if err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	if got, err = b.EnsureImage(ctx, "agent:base", ref); err != nil || got != ref {
		t.Fatalf("EnsureImage after capture = %q, %v; want %q", got, err, ref)
	}

	// An unlaunchable delta (the "committed but never unpacked into the snapshotter" class) must NOT
	// be returned — a capture that produces an unlaunchable image is a failure, not a silent pass.
	b.MarkUnlaunchable(ref)
	if got, err = b.EnsureImage(ctx, "agent:base", ref); err != nil || got != "agent:base" {
		t.Fatalf("EnsureImage with an unlaunchable delta = %q, %v; want agent:base", got, err)
	}
}

func TestStartAgentFromDeltaReplaysContent(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if err := b.AgentWrite("sp1", "/work/a", []byte("A")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	ref, err := b.CaptureDelta(ctx, h)
	if err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	h2, err := b.StartPod(ctx, runtime.PodSpec{ID: "sp1", SidecarImage: "sidecar:test"})
	if err != nil {
		t.Fatalf("StartPod (resume): %v", err)
	}
	if err := b.StartAgent(ctx, h2, runtime.AgentSpec{Image: ref}); err != nil {
		t.Fatalf("StartAgent (resume): %v", err)
	}
	if string(b.RootfsView("sp1")["/work/a"]) != "A" {
		t.Fatalf("resume from %s did not replay the delta content: %v", ref, b.RootfsView("sp1"))
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if err := b.AgentWrite("sp1", "/work/a", []byte("A")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	if _, err := b.CaptureDeltaAs(ctx, h, "sp1"); err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	var buf bytes.Buffer
	if err := b.ExportDelta(ctx, "sp1", &buf); err != nil {
		t.Fatalf("ExportDelta: %v", err)
	}

	dst := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(dst.Close)
	ref, err := dst.ImportDelta(ctx, "sp1", "agent:base", &buf)
	if err != nil {
		t.Fatalf("ImportDelta: %v", err)
	}
	if ref != runtime.DeltaTag("sp1") {
		t.Fatalf("ImportDelta ref = %q, want %q", ref, runtime.DeltaTag("sp1"))
	}
	content, ok := dst.ImageContent(ref)
	if !ok || string(content["/work/a"]) != "A" {
		t.Fatalf("imported content = %v, want /work/a=A", content)
	}
	if got := dst.ImportBaseRefs(); len(got) != 1 || got[0] != "agent:base" {
		t.Fatalf("ImportBaseRefs = %v, want [agent:base]", got)
	}
}

func TestReleaseDeltaRemovesTheImage(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	ref, err := b.CaptureDeltaAs(ctx, h, "sp1")
	if err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	if err := b.ReleaseDelta(ctx, "sp1"); err != nil {
		t.Fatalf("ReleaseDelta: %v", err)
	}
	if _, ok := b.ImageContent(ref); ok {
		t.Fatal("ReleaseDelta must remove the delta image")
	}
	if got := b.ReleasedSpawns(); len(got) != 1 || got[0] != "sp1" {
		t.Fatalf("ReleasedSpawns = %v, want [sp1]", got)
	}
}

func TestDeltaSize(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New(fakepod.WithBaseImage("agent:base", 3))
	t.Cleanup(b.Close)
	h := captureHandle(t, b, "sp1")
	if err := b.AgentWrite("sp1", "/work/a", []byte("12345")); err != nil {
		t.Fatalf("AgentWrite: %v", err)
	}
	if _, err := b.CaptureDeltaAs(ctx, h, "sp1"); err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	n, err := b.DeltaSize(ctx, "sp1")
	if err != nil {
		t.Fatalf("DeltaSize: %v", err)
	}
	if n != 5 {
		t.Fatalf("DeltaSize = %d, want 5", n)
	}
	if n, err = b.DeltaSize(ctx, "nope"); err != nil || n != 0 {
		t.Fatalf("DeltaSize(unknown) = %d, %v; want 0, nil", n, err)
	}
}
