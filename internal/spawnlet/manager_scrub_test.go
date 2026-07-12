package spawnlet

// manager_scrub_test.go: tests for the live capture-time scrub seam (spec §3, task .12).
//
// Test matrix:
//   SC1: scrubFn is called with the default DeltaScrubPaths before CaptureDelta.
//   SC2: scrubFn is NOT called when DeltaCapture=false.
//   SC3: scrubFn failure is non-fatal (CaptureDelta still proceeds).
//   SC4: Scrub happens before capture (ordering via ops on the fake).
//   SC5: The default scrub removes /tmp noise but restores the directory before capture commits.
//   SC6: On the gate path the scrub runs BEFORE pod.Pause (sp-2tx8.2.1) — an exec cannot enter a
//        paused container.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"spawnery/internal/runtime/fakepod"
)

// SC1: Scrub is called with the configured scrub paths on Suspend.
func TestScrubCalledWithDefaultPaths(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	var scrubCalled bool
	var scrubPaths []string
	var scrubBeforeCapture bool
	m.scrubFn = func(_ context.Context, _ string, paths []string) error {
		scrubCalled = true
		scrubPaths = paths
		// At this point CaptureDelta has NOT been called yet (capture happens after scrub).
		scrubBeforeCapture = lastOf(fb.CapturedRefs()) == ""
		return nil
	}

	sp, err := m.Create(ctx, "sp-scrub", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if !scrubCalled {
		t.Fatal("scrubFn was not called on Suspend with DeltaCapture=true")
	}
	// Default paths must include the three standard scrub targets.
	defaultExpected := []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"}
	if len(scrubPaths) != len(defaultExpected) {
		t.Fatalf("scrubPaths = %v, want %v", scrubPaths, defaultExpected)
	}
	for i, p := range defaultExpected {
		if scrubPaths[i] != p {
			t.Errorf("scrubPaths[%d] = %q, want %q", i, scrubPaths[i], p)
		}
	}
	if !scrubBeforeCapture {
		t.Fatal("scrub must happen BEFORE CaptureDelta")
	}
}

// SC2: scrubFn is NOT called when DeltaCapture=false.
func TestScrubNotCalledWhenCaptureDisabled(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: false,
	})

	var scrubCalled bool
	m.scrubFn = func(_ context.Context, _ string, _ []string) error {
		scrubCalled = true
		return nil
	}

	sp, err := m.Create(ctx, "sp-noscrub", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if scrubCalled {
		t.Fatal("scrubFn must NOT be called when DeltaCapture=false")
	}
}

// SC3: scrubFn failure is non-fatal — CaptureDelta must still be called.
func TestScrubFailureIsNonFatal(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	m.scrubFn = func(_ context.Context, _ string, _ []string) error {
		return errors.New("rm: permission denied (injected)")
	}

	sp, err := m.Create(ctx, "sp-scrub-fail", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// CaptureDelta must still have been called despite scrub failure.
	if lastOf(fb.CapturedRefs()) == "" {
		t.Fatal("CaptureDelta must be called even when scrubFn fails")
	}
}

// SC4: Ordering — scrub happens before capture (injected via ops recording).
func TestScrubHappensBeforeCapture(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	// scrubIdx records the position in fb.Ops() when the scrubFn fires.
	// fb.Ops() grows on every backend call; scrub fires BEFORE the capture op is appended.
	var scrubFiredAtOpsLen int = -1
	m.scrubFn = func(_ context.Context, _ string, _ []string) error {
		scrubFiredAtOpsLen = len(fb.Ops())
		return nil
	}

	sp, err := m.Create(ctx, "sp-order", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := m.Suspend(ctx, sp.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// scrubFn must have fired.
	if scrubFiredAtOpsLen < 0 {
		t.Fatal("scrubFn was not called")
	}
	captureIdx := opsIndex(fb.Ops(), "capture:sp-order")
	if captureIdx < 0 {
		t.Fatalf("capture not in ops; ops=%v", fb.Ops())
	}
	// scrubFn must have fired before the capture op was appended.
	if scrubFiredAtOpsLen > captureIdx {
		t.Fatalf("scrub fired at ops-len=%d but capture is at index=%d; scrub should be first",
			scrubFiredAtOpsLen, captureIdx)
	}
}

// SC5: The default scrub removes /tmp noise but restores the directory before
// CaptureDelta commits the image. tmux needs a standard sticky /tmp for its
// socket directory after resume.
func TestDefaultScrubCommandsRestoreTmpBeforeCapture(t *testing.T) {
	paths := []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"}

	got := defaultScrubCommands(paths)
	want := [][]string{
		{"rm", "-rf", "/var/cache/apt", "/var/lib/apt/lists", "/tmp"},
		{"mkdir", "-p", "/tmp"},
		{"chmod", "1777", "/tmp"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultScrubCommands(%v) = %v, want %v", paths, got, want)
	}
}

// SC6: on the GATE path the scrub runs inside SnapshotForSuspend, BEFORE pod.Pause — an exec cannot
// enter a paused container, and the capture that follows must see a frozen one (sp-2tx8.2.1).
func TestGateScrubsBeforePause(t *testing.T) {
	ctx := context.Background()
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	var scrubbedWhilePaused bool
	var scrubCalled bool
	m.scrubFn = func(_ context.Context, _ string, _ []string) error {
		scrubCalled = true
		scrubbedWhilePaused = fb.State("sp-gate-scrub", "agent") == fakepod.StatePaused
		return nil
	}

	sp, err := m.Create(ctx, "sp-gate-scrub", writeApp(t), "model", "", "", 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.SnapshotForSuspend(ctx, sp.ID, nil); err != nil {
		t.Fatalf("SnapshotForSuspend: %v", err)
	}
	if !scrubCalled {
		t.Fatal("the suspend gate must scrub (DeltaCapture=true)")
	}
	if scrubbedWhilePaused {
		t.Fatal("the gate scrubbed a PAUSED agent — an exec cannot enter one; the scrub must run before Pause")
	}
	if fb.PauseCount() != 1 {
		t.Fatalf("the gate must pause the agent exactly once; pauseCount=%d", fb.PauseCount())
	}

	if _, err := m.FinishSuspend(ctx, sp.ID, false, nil); err != nil {
		t.Fatalf("FinishSuspend: %v", err)
	}
}
