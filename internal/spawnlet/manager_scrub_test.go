package spawnlet

// manager_scrub_test.go: tests for the live capture-time scrub seam (spec §3, task .12).
//
// Test matrix:
//   SC1: scrubFn is called with the default DeltaScrubPaths before CaptureDelta.
//   SC2: scrubFn is NOT called when DeltaCapture=false.
//   SC3: scrubFn failure is non-fatal (CaptureDelta still proceeds).
//   SC4: Scrub happens before capture (ordering via ops on the fake).

import (
	"context"
	"errors"
	"testing"
)

// SC1: Scrub is called with the configured scrub paths on Suspend.
func TestScrubCalledWithDefaultPaths(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
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
		scrubBeforeCapture = fb.capturedRef == ""
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
	fb := &fakePodBackend{}
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
	fb := &fakePodBackend{}
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
	if fb.capturedRef == "" {
		t.Fatal("CaptureDelta must be called even when scrubFn fails")
	}
}

// SC4: Ordering — scrub happens before capture (injected via ops recording).
func TestScrubHappensBeforeCapture(t *testing.T) {
	ctx := context.Background()
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
		DeltaCapture: true,
	})

	// scrubIdx records the position in fb.ops when the scrubFn fires.
	// fb.ops grows on CaptureDelta ("capture:id") and Stop ("stop"); scrub fires BEFORE capture.
	var scrubFiredAtOpsLen int = -1
	m.scrubFn = func(_ context.Context, _ string, _ []string) error {
		scrubFiredAtOpsLen = len(fb.ops)
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
	captureIdx := opsIndex(fb.ops, "capture:sp-order")
	if captureIdx < 0 {
		t.Fatalf("capture not in ops; ops=%v", fb.ops)
	}
	// scrubFn must have fired before the capture op was appended.
	if scrubFiredAtOpsLen > captureIdx {
		t.Fatalf("scrub fired at ops-len=%d but capture is at index=%d; scrub should be first",
			scrubFiredAtOpsLen, captureIdx)
	}
}
