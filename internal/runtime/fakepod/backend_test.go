package fakepod_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"spawnery/internal/runtime"
	"spawnery/internal/runtime/fakepod"
)

// startPod is the shared fixture: a running pod with a running agent that has one mount at /data.
func startPod(t *testing.T, b *fakepod.Backend, id string) *runtime.PodHandle {
	t.Helper()
	ctx := context.Background()
	h, err := b.StartPod(ctx, runtime.PodSpec{
		ID:           id,
		SidecarImage: "sidecar:test",
		Labels: map[string]string{
			runtime.LabelManaged:    "true",
			runtime.LabelSpawnID:    id,
			runtime.LabelGeneration: "7",
			runtime.LabelNodeID:     "node-a",
		},
	})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{
		Image:  "agent:base",
		Mounts: []runtime.Mount{{HostPath: "/host/data", ContainerPath: "/data"}},
	}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	return h
}

func TestStartPodDoesNotFillSpawnID(t *testing.T) {
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	// Both real backends leave SpawnID empty — the Manager sets it. Filling it in would mask a bug.
	if h.SpawnID != "" {
		t.Fatalf("PodHandle.SpawnID = %q, want empty", h.SpawnID)
	}
	if h.SidecarID == "" || h.SandboxID == "" || h.AgentID == "" {
		t.Fatalf("handle ids not populated: %+v", h)
	}
	if got := b.State("sp1", "agent"); got != fakepod.StateRunning {
		t.Fatalf("agent state = %s, want running", got)
	}
}

func TestStartAgentWithoutSandboxFails(t *testing.T) {
	b := fakepod.New()
	t.Cleanup(b.Close)
	err := b.StartAgent(context.Background(), &runtime.PodHandle{SidecarID: "nope"},
		runtime.AgentSpec{Image: "agent:base"})
	if err == nil {
		t.Fatal("StartAgent into a non-existent sandbox must fail, got nil")
	}
}

func TestAttachRequiresRunningAgent(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")

	s, err := b.Attach(ctx, h)
	if err != nil {
		t.Fatalf("Attach on a running agent: %v", err)
	}
	_ = s.Close()

	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := b.Attach(ctx, h); err == nil {
		t.Fatal("Attach to a removed agent must fail, got nil")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("second Stop must be a no-op, got %v", err)
	}
	if got := b.State("sp1", "agent"); got != fakepod.StateRemoved {
		t.Fatalf("agent state after Stop = %s, want removed", got)
	}
	if got := b.LastStopHandle(); got == nil || got.SandboxID != h.SandboxID {
		t.Fatalf("LastStopHandle = %+v, want handle with sandbox %q", got, h.SandboxID)
	}
}

func TestListManagedRoundTripsLabels(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	pods, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("ListManaged = %d pods, want 1", len(pods))
	}
	p := pods[0]
	if p.SpawnID != "sp1" || p.Generation != 7 || p.NodeID != "node-a" {
		t.Fatalf("ManagedPod = %+v, want spawn sp1 gen 7 node-a", p)
	}
	if p.AgentID != h.AgentID || p.SidecarID != h.SidecarID || p.SandboxID != h.SandboxID {
		t.Fatalf("ManagedPod ids = %+v, want %+v", p, h)
	}
	if p.PodIP == "" || p.PodIP != h.PodIP {
		t.Fatalf("ManagedPod PodIP = %q, want %q", p.PodIP, h.PodIP)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	pods, err = b.ListManaged(ctx)
	if err != nil {
		t.Fatalf("ListManaged after Stop: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("ListManaged after Stop = %d pods, want 0", len(pods))
	}
}

func TestFailOnInjectsErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	b := fakepod.New(fakepod.WithFailOn(fakepod.OpStartAgent, boom))
	t.Cleanup(b.Close)
	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "sp1", SidecarImage: "sidecar:test"})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{Image: "agent:base"}); !errors.Is(err, boom) {
		t.Fatalf("StartAgent err = %v, want %v", err, boom)
	}
	b.ClearFailOn(fakepod.OpStartAgent)
	if err := b.StartAgent(ctx, h, runtime.AgentSpec{Image: "agent:base"}); err != nil {
		t.Fatalf("StartAgent after ClearFailOn: %v", err)
	}
}

func TestOpsLog(t *testing.T) {
	ctx := context.Background()
	b := fakepod.New()
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := []string{"startpod:sp1", "startagent:sp1", "stop:sp1"}
	got := b.Ops()
	if len(got) != len(want) {
		t.Fatalf("Ops = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Ops = %v, want %v", got, want)
		}
	}
}

func TestAttachScript(t *testing.T) {
	b := fakepod.New(fakepod.WithAttachScript(func(_ io.Reader, w io.Writer) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(b.Close)
	h := startPod(t, b, "sp1")
	s, err := b.Attach(context.Background(), h)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = s.Close() }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(s.Stdout, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("script output = %q, want hello", buf)
	}
}

// deltaSizer mirrors the (unexported) interface Manager.DeltaSize type-asserts m.pod against.
type deltaSizer interface {
	DeltaSize(ctx context.Context, spawnID string) (int64, error)
}

func TestWithoutDeltaSizeHidesTheMethod(t *testing.T) {
	b := fakepod.New()
	t.Cleanup(b.Close)
	if _, ok := any(b).(deltaSizer); !ok {
		t.Fatal("*fakepod.Backend must satisfy deltaSizer")
	}
	// Embedding the PodBackend INTERFACE (not *Backend) is what hides DeltaSize: the quota tests need
	// a backend whose type assertion FAILS, yielding "unknown size".
	if _, ok := any(fakepod.WithoutDeltaSize(b)).(deltaSizer); ok {
		t.Fatal("WithoutDeltaSize(b) must NOT satisfy deltaSizer")
	}
	var _ runtime.PodBackend = fakepod.WithoutDeltaSize(b) // still a full PodBackend
}
