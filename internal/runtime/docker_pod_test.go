package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestDockerPodBackendStartPodStartAgentStop(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()

	res := Resources{MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PidsLimit: 128}
	h, err := b.StartPod(ctx, PodSpec{
		ID:           "sp1",
		SidecarImage: "sidecar-img",
		SidecarEnv:   []string{"OPENROUTER_API_KEY=k", "SIDECAR_ADDR=127.0.0.1:8080"},
		Resources:    res,
		Runtime:      "runsc",
	})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if h.SidecarID != "fake-1" {
		t.Fatalf("SidecarID = %q", h.SidecarID)
	}
	if h.NetnsPath != "/proc/4242/ns/net" {
		t.Fatalf("NetnsPath = %q", h.NetnsPath)
	}
	if h.PodIP != "172.17.0.99" {
		t.Fatalf("PodIP = %q", h.PodIP)
	}
	if h.AgentID != "" {
		t.Fatalf("AgentID must be empty until StartAgent, got %q", h.AgentID)
	}
	if len(f.Started) != 1 {
		t.Fatalf("want 1 started (sidecar), got %d", len(f.Started))
	}
	sc := f.Started[0]
	if sc.Image != "sidecar-img" || sc.MemoryBytes != 512<<20 || sc.NanoCPUs != 2_000_000_000 || sc.PidsLimit != 128 || sc.Runtime != "runsc" {
		t.Fatalf("sidecar spec wrong: %+v", sc)
	}

	err = b.StartAgent(ctx, h, AgentSpec{
		Image:          "agent-img",
		Env:            []string{"SPAWN_MODEL=m"},
		Mounts:         []Mount{{HostPath: "/h", ContainerPath: "/app"}},
		Resources:      res,
		Runtime:        "runsc",
		DropAllCaps:    true,
		ReadonlyRootfs: true,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if h.AgentID != "fake-2" {
		t.Fatalf("AgentID = %q", h.AgentID)
	}
	if len(f.Started) != 2 {
		t.Fatalf("want 2 started, got %d", len(f.Started))
	}
	ag := f.Started[1]
	if ag.Image != "agent-img" || ag.NetnsOf != "fake-1" || !ag.DropAllCaps || !ag.ReadonlyRootfs || ag.Runtime != "runsc" {
		t.Fatalf("agent spec wrong: %+v", ag)
	}

	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !f.Stopped["fake-1"] || !f.Stopped["fake-2"] {
		t.Fatalf("both containers must be stopped; stopped=%v", f.Stopped)
	}
}

func TestDockerPodBackendStopSkipsEmptyAgentID(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()
	h, err := b.StartPod(ctx, PodSpec{ID: "sp1", SidecarImage: "s", Resources: Resources{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if !f.Stopped["fake-1"] {
		t.Fatal("sidecar must be stopped")
	}
	if f.Stopped[""] {
		t.Fatal("must not StopContainer with an empty agent id")
	}
}

// errOnRuntime errors when a non-default Runtime is requested (simulates broken/missing runsc).
type errOnRuntime struct{ *FakeRuntime }

func (r errOnRuntime) StartContainer(ctx context.Context, s ContainerSpec) (string, error) {
	if s.Runtime != "" {
		return "", errors.New("runsc not installed")
	}
	return r.FakeRuntime.StartContainer(ctx, s)
}

type errOnPID struct{ *FakeRuntime }

func (r errOnPID) ContainerPID(ctx context.Context, id string) (int, error) {
	return 0, errors.New("no pid")
}

type errOnIP struct{ *FakeRuntime }

func (r errOnIP) ContainerIP(ctx context.Context, id string) (string, error) {
	return "", errors.New("no ip")
}

func TestDockerPodBackendStartPodCleansUpSidecarOnFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("pid failure", func(t *testing.T) {
		f := NewFake()
		_, err := NewDockerPodBackend(errOnPID{f}, "", "smoke").StartPod(ctx, PodSpec{SidecarImage: "s"})
		if err == nil {
			t.Fatal("expected error when ContainerPID fails")
		}
		if !f.Stopped["fake-1"] {
			t.Fatalf("sidecar must be stopped on pid failure; stopped=%v", f.Stopped)
		}
	})
	t.Run("ip failure", func(t *testing.T) {
		f := NewFake()
		_, err := NewDockerPodBackend(errOnIP{f}, "", "smoke").StartPod(ctx, PodSpec{SidecarImage: "s"})
		if err == nil {
			t.Fatal("expected error when ContainerIP fails")
		}
		if !f.Stopped["fake-1"] {
			t.Fatalf("sidecar must be stopped on ip failure; stopped=%v", f.Stopped)
		}
	})
}

func TestDockerPodBackendAttachUsesRuntimeAttach(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	att, err := b.Attach(context.Background(), &PodHandle{AgentID: "fake-1"})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if att == nil || att.Stdin == nil || att.Stdout == nil {
		t.Fatalf("Attach returned an incomplete stream: %+v", att)
	}
	// The fake's stream is a synchronous io.Pipe (Write blocks until Read), so write concurrently —
	// matching how the relay actually drives it (separate stdin->stdout goroutines).
	werr := make(chan error, 1)
	go func() {
		_, err := att.Stdin.Write([]byte("ping"))
		werr <- err
	}()
	buf := make([]byte, 4)
	if _, err := att.Stdout.Read(buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	if err := <-werr; err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = att.Close()
}

func TestDockerPodBackendPreflight(t *testing.T) {
	ctx := context.Background()
	if err := NewDockerPodBackend(NewFake(), "", "smoke").Preflight(ctx); err != nil {
		t.Fatalf("empty runtime should preflight nil, got %v", err)
	}
	if err := NewDockerPodBackend(NewFake(), "runsc", "smoke").Preflight(ctx); err != nil {
		t.Fatalf("healthy runtime should preflight nil, got %v", err)
	}
	if err := NewDockerPodBackend(errOnRuntime{NewFake()}, "runsc", "smoke").Preflight(ctx); err == nil {
		t.Fatal("broken runtime must fail preflight")
	}
}
