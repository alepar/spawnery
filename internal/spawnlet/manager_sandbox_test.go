package spawnlet

import (
	"context"
	"io"
	"testing"

	"spawnery/internal/runtime"
)

// fakePodBackend records Stop's handle and returns a sandbox-bearing handle from StartPod.
// It also implements the delta-capture methods so the manager tests compile against the full
// PodBackend interface (ResolveImageDigest/EnsureImage/CaptureDelta/ReleaseDelta).
type fakePodBackend struct {
	stopped   *runtime.PodHandle
	agentSpec runtime.AgentSpec // captured by StartAgent
	podSpec   runtime.PodSpec   // captured by StartPod

	// Delta-capture controls (overridable per test).
	capturedRef    string   // set by CaptureDelta if called (last capture)
	capturedRefs   []string // ordered list of all captured refs
	captureErr     error    // if non-nil, CaptureDelta returns this
	resolveDigest  string   // returned by ResolveImageDigest
	ensureImageRef string   // returned by EnsureImage (empty -> returns baseRef)

	// GC tracking.
	releasedSpawn string // set by ReleaseDelta with the spawnID

	// ops records call order for ordering assertions, e.g. "capture:<id>", "stop".
	ops []string

	// Pause/Unpause tracking (dedicated fields — NOT in ops to avoid shifting indices
	// that existing scrub/reconcile tests assert against).
	paused        bool   // true after Pause, false after Unpause
	pauseCount    int    // total Pause calls
	unpauseCount  int    // total Unpause calls
	pausedAgentID string // AgentID from the most-recent Pause call
	pauseErr      error  // if non-nil, Pause returns this error (non-fatal pause test)

	// listManaged controls ListManaged return value (for ReapOrphans tests).
	listManaged []runtime.ManagedPod

	// deltaSizeMB, if > 0, makes this backend implement the deltaSizer interface.
	// Call enableDeltaSize() after construction to activate it.
	deltaSizeMB int64
}

func (f *fakePodBackend) Ping(context.Context) error      { return nil }
func (f *fakePodBackend) Preflight(context.Context) error { return nil }
func (f *fakePodBackend) StartPod(_ context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	f.podSpec = spec
	return &runtime.PodHandle{PodIP: "10.0.0.5", NetnsPath: "/proc/7/ns/net", SidecarID: "sc", SandboxID: "sandbox-x"}, nil
}
func (f *fakePodBackend) StartAgent(_ context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	f.agentSpec = spec
	h.AgentID = "ag"
	return nil
}
func (f *fakePodBackend) Stop(_ context.Context, h *runtime.PodHandle) error {
	f.stopped = h
	f.ops = append(f.ops, "stop")
	return nil
}
func (f *fakePodBackend) Attach(_ context.Context, _ *runtime.PodHandle) (*runtime.AttachedStream, error) {
	pr, pw := io.Pipe()
	return &runtime.AttachedStream{Stdin: pw, Stdout: pr, Close: pw.Close}, nil
}
func (f *fakePodBackend) ListManaged(_ context.Context) ([]runtime.ManagedPod, error) {
	return f.listManaged, nil
}

func (f *fakePodBackend) ResolveImageDigest(_ context.Context, _ string) (string, error) {
	return f.resolveDigest, nil
}
func (f *fakePodBackend) EnsureImage(_ context.Context, baseRef, _ string) (string, error) {
	if f.ensureImageRef != "" {
		return f.ensureImageRef, nil
	}
	return baseRef, nil
}
func (f *fakePodBackend) CaptureDelta(_ context.Context, h *runtime.PodHandle) (string, error) {
	return f.CaptureDeltaAs(context.Background(), h, h.SpawnID)
}
func (f *fakePodBackend) CaptureDeltaAs(_ context.Context, h *runtime.PodHandle, targetSpawnID string) (string, error) {
	if f.captureErr != nil {
		return "", f.captureErr
	}
	ref := runtime.DeltaTag(targetSpawnID)
	f.capturedRef = ref
	f.capturedRefs = append(f.capturedRefs, ref)
	f.ops = append(f.ops, "capture:"+targetSpawnID)
	return ref, nil
}
func (f *fakePodBackend) ReleaseDelta(_ context.Context, spawnID string) error {
	f.releasedSpawn = spawnID
	f.ops = append(f.ops, "release:"+spawnID)
	return nil
}
func (f *fakePodBackend) ExportDelta(_ context.Context, spawnID string, w io.Writer) error {
	f.ops = append(f.ops, "export:"+spawnID)
	_, err := w.Write([]byte(runtime.DeltaTag(spawnID)))
	return err
}
func (f *fakePodBackend) ImportDelta(_ context.Context, spawnID, _ string, r io.Reader) (string, error) {
	f.ops = append(f.ops, "import:"+spawnID)
	_, _ = io.Copy(io.Discard, r)
	return runtime.DeltaTag(spawnID), nil
}

// Pause records the call. Always increments pauseCount and captures pausedAgentID so tests can
// assert the call was attempted even when it fails. Sets paused=true only on success.
// Returns pauseErr if set (non-fatal in the gate — the manager logs and snapshots anyway).
func (f *fakePodBackend) Pause(_ context.Context, h *runtime.PodHandle) error {
	f.pauseCount++
	f.pausedAgentID = h.AgentID
	if f.pauseErr != nil {
		return f.pauseErr
	}
	f.paused = true
	return nil
}

// Unpause records the call and sets paused=false.
func (f *fakePodBackend) Unpause(_ context.Context, _ *runtime.PodHandle) error {
	f.paused = false
	f.unpauseCount++
	return nil
}

// DeltaSize implements the optional deltaSizer interface used by CheckQuotas.
// It is defined on fakePodBackend so tests can enable it by setting deltaSizeMB > 0.
// The interface assertion in CheckQuotas will succeed for any *fakePodBackend because
// the method always exists; tests that want a "no size source" backend use noSizeFakeBackend.
func (f *fakePodBackend) DeltaSize(_ context.Context, _ string) (int64, error) {
	return f.deltaSizeMB << 20, nil
}

func TestManagerThreadsSandboxID(t *testing.T) {
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	fb := &fakePodBackend{}
	m.pod = fb // white-box: replace the Docker backend with the fake

	sp, err := m.Create(context.Background(), "spx", "../../examples/secret-app", "model", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sp.SandboxID != "sandbox-x" {
		t.Fatalf("Spawn.SandboxID = %q, want sandbox-x", sp.SandboxID)
	}
	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatal(err)
	}
	if fb.stopped == nil || fb.stopped.SandboxID != "sandbox-x" {
		t.Fatalf("Stop handle SandboxID = %+v, want sandbox-x", fb.stopped)
	}
}
