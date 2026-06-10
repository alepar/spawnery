package spawnlet

import (
	"context"
	"io"
	"testing"

	"spawnery/internal/runtime"
)

// fakePodBackend records Stop's handle and returns a sandbox-bearing handle from StartPod.
type fakePodBackend struct {
	stopped   *runtime.PodHandle
	agentSpec runtime.AgentSpec // captured by StartAgent
	podSpec   runtime.PodSpec   // captured by StartPod
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
	return nil
}
func (f *fakePodBackend) Attach(_ context.Context, _ *runtime.PodHandle) (*runtime.AttachedStream, error) {
	pr, pw := io.Pipe()
	return &runtime.AttachedStream{Stdin: pw, Stdout: pr, Close: pw.Close}, nil
}
func (f *fakePodBackend) ListManaged(_ context.Context) ([]runtime.ManagedPod, error) { return nil, nil }

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
