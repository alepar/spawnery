package runtime

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
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
		Image:       "agent-img",
		Env:         []string{"SPAWN_MODEL=m"},
		Mounts:      []Mount{{HostPath: "/h", ContainerPath: "/app"}},
		Resources:   res,
		Runtime:     "runsc",
		DropAllCaps: true,
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
	// AgentSpec.DropAllCaps=true is mapped to ContainerSpec.CapPolicy=CapDropAll by StartAgent.
	if ag.Image != "agent-img" || ag.NetnsOf != "fake-1" || ag.CapPolicy != CapDropAll || ag.Runtime != "runsc" {
		t.Fatalf("agent spec wrong: %+v", ag)
	}
	if got := envValue(ag.Env, "TMUX_TMPDIR"); got != "/dev/shm" {
		t.Fatalf("agent TMUX_TMPDIR = %q, want /dev/shm", got)
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

func TestDockerPodBackendStartPodToleratesMissingIPAndPID(t *testing.T) {
	ctx := context.Background()
	t.Run("missing ip", func(t *testing.T) {
		f := NewFake()
		h, err := NewDockerPodBackend(errOnIP{f}, "", "smoke").StartPod(ctx, PodSpec{SidecarImage: "s"})
		if err != nil {
			t.Fatalf("StartPod must tolerate a missing IP, got %v", err)
		}
		if h.PodIP != "" {
			t.Fatalf("PodIP = %q, want empty", h.PodIP)
		}
		if f.Stopped["fake-1"] {
			t.Fatal("sidecar must NOT be stopped when the IP is merely unavailable")
		}
	})
	t.Run("missing pid", func(t *testing.T) {
		f := NewFake()
		h, err := NewDockerPodBackend(errOnPID{f}, "", "smoke").StartPod(ctx, PodSpec{SidecarImage: "s"})
		if err != nil {
			t.Fatalf("StartPod must tolerate a missing PID, got %v", err)
		}
		if h.NetnsPath != "" {
			t.Fatalf("NetnsPath = %q, want empty", h.NetnsPath)
		}
	})
}

func TestDockerPodBackendAttachDialsTCP(t *testing.T) {
	// A loopback listener stands in for the in-pod acpadapter; the backend dials podIP:port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Echo server: read 4 bytes, write them back.
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		if _, rerr := c.Read(buf); rerr == nil {
			_, _ = c.Write(buf)
		}
	}()

	b := NewDockerPodBackend(NewFake(), "", "smoke")
	b.acpPort = port // white-box: override the fixed ACPPort for the test
	att, err := b.Attach(context.Background(), &PodHandle{AgentID: "ag", PodIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer att.Close()
	if _, err := att.Stdin.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := att.Stdout.Read(buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestDockerPodBackendAttachRequiresIP(t *testing.T) {
	b := NewDockerPodBackend(NewFake(), "", "smoke")
	if _, err := b.Attach(context.Background(), &PodHandle{AgentID: "ag"}); err == nil {
		t.Fatal("Attach must fail when the pod has no IP")
	}
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

// --- Delta-capture method tests (A1–A5) -------------------------------------

// A1: CaptureDelta commits the agent container and returns the delta tag.
func TestCaptureDeltaCommitsAndTags(t *testing.T) {
	f := NewFake()
	const baseRef = "spawnery/agent:dev"
	// Seed the base image with 5 layers so the guard passes (committed = 6).
	f.Images[baseRef] = ImageInfo{ID: "sha256:base", Layers: 5}
	// Also ensure the last-started image matches so CommitContainer derives baseLayers correctly.
	f.Started = append(f.Started, ContainerSpec{Image: baseRef})

	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()
	h := &PodHandle{SpawnID: "sp1", AgentID: "agent-1", BaseImageRef: baseRef}

	ref, err := b.CaptureDelta(ctx, h)
	if err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}
	if ref != "spawnery/delta:sp1" {
		t.Fatalf("delta tag = %q, want spawnery/delta:sp1", ref)
	}
	if len(f.Committed) != 1 {
		t.Fatalf("want 1 commit, got %d", len(f.Committed))
	}
	if f.Committed[0].ContainerID != "agent-1" || f.Committed[0].Ref != "spawnery/delta:sp1" {
		t.Fatalf("commit wrong: %+v", f.Committed[0])
	}
	// Verify the committed image is inspectable (CommitContainer seeded it).
	ni, ok, err := f.InspectImage(ctx, "spawnery/delta:sp1")
	if err != nil || !ok {
		t.Fatalf("committed image not inspectable: ok=%v err=%v", ok, err)
	}
	if ni.Layers <= 5 {
		t.Fatalf("committed image layers = %d, want > 5 (base)", ni.Layers)
	}
	if !f.Stopped["agent-1"] {
		t.Fatal("CaptureDelta must stop the source agent container after suspend capture")
	}
}

func TestCaptureDeltaAsCommitsWithoutStoppingSourceContainer(t *testing.T) {
	f := NewFake()
	const baseRef = "spawnery/agent:dev"
	f.Images[baseRef] = ImageInfo{ID: "sha256:base", Layers: 5}
	f.Started = append(f.Started, ContainerSpec{Image: baseRef})

	b := NewDockerPodBackend(f, "", "smoke")
	h := &PodHandle{SpawnID: "sp-source", AgentID: "agent-1", BaseImageRef: baseRef}

	ref, err := b.CaptureDeltaAs(context.Background(), h, "sp-fork")
	if err != nil {
		t.Fatalf("CaptureDeltaAs: %v", err)
	}
	if ref != "spawnery/delta:sp-fork" {
		t.Fatalf("delta tag = %q, want spawnery/delta:sp-fork", ref)
	}
	if f.Stopped["agent-1"] {
		t.Fatal("CaptureDeltaAs must not stop the source agent container")
	}
}

// A2: CaptureDelta returns an error when the committed image has <= base layers (moby#47065 guard).
func TestCaptureDeltaLayerGuard(t *testing.T) {
	f := NewFake()
	const baseRef = "spawnery/agent:dev"
	f.Images[baseRef] = ImageInfo{ID: "sha256:base", Layers: 5}
	f.Started = append(f.Started, ContainerSpec{Image: baseRef})
	// Force CommitContainer to produce only 3 layers (< base = 5).
	f.CommitLayers = 3

	b := NewDockerPodBackend(f, "", "smoke")
	h := &PodHandle{SpawnID: "sp-guard", AgentID: "ag", BaseImageRef: baseRef}

	_, err := b.CaptureDelta(context.Background(), h)
	if err == nil {
		t.Fatal("CaptureDelta must error when committed layers <= base (moby#47065 guard)")
	}
	// Error should mention the guard.
	if !strings.Contains(err.Error(), "guard") && !strings.Contains(err.Error(), "47065") {
		t.Fatalf("error should mention the guard: %v", err)
	}
}

// A3: EnsureImage returns the delta tag when present, base ref otherwise.
func TestEnsureImagePrefersDeltaTag(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()
	const base = "spawnery/agent:dev"
	const delta = "spawnery/delta:sp1"

	// Without the delta image present → returns base.
	got, err := b.EnsureImage(ctx, base, delta)
	if err != nil {
		t.Fatalf("EnsureImage (no delta): %v", err)
	}
	if got != base {
		t.Fatalf("EnsureImage (no delta) = %q, want %q", got, base)
	}

	// Seed the delta image → returns delta.
	f.Images[delta] = ImageInfo{ID: "sha256:delta", Layers: 6}
	got, err = b.EnsureImage(ctx, base, delta)
	if err != nil {
		t.Fatalf("EnsureImage (with delta): %v", err)
	}
	if got != delta {
		t.Fatalf("EnsureImage (with delta) = %q, want %q", got, delta)
	}
}

// A4: ResolveImageDigest returns RepoDigests[0] when present, falls back to ID.
func TestResolveImageDigest(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()
	const ref = "spawnery/agent:dev"

	// With a RepoDigest → returns the digest.
	f.Images[ref] = ImageInfo{ID: "sha256:id", RepoDigests: []string{"spawnery/agent@sha256:abc"}, Layers: 5}
	got, err := b.ResolveImageDigest(ctx, ref)
	if err != nil {
		t.Fatalf("ResolveImageDigest: %v", err)
	}
	if got != "spawnery/agent@sha256:abc" {
		t.Fatalf("ResolveImageDigest = %q, want digest", got)
	}

	// Without RepoDigests → falls back to ID.
	f.Images[ref] = ImageInfo{ID: "sha256:id-only", Layers: 5}
	got, err = b.ResolveImageDigest(ctx, ref)
	if err != nil {
		t.Fatalf("ResolveImageDigest (id fallback): %v", err)
	}
	if got != "sha256:id-only" {
		t.Fatalf("ResolveImageDigest (id fallback) = %q, want sha256:id-only", got)
	}
}

// A5: ReleaseDelta calls RemoveImage on the delta tag.
func TestReleaseDeltaRemovesTag(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	const delta = "spawnery/delta:sp1"
	f.Images[delta] = ImageInfo{ID: "sha256:delta"}

	if err := b.ReleaseDelta(context.Background(), "sp1"); err != nil {
		t.Fatalf("ReleaseDelta: %v", err)
	}
	if len(f.Removed) != 1 || f.Removed[0] != delta {
		t.Fatalf("Removed = %v, want [%s]", f.Removed, delta)
	}
}

func TestExportDeltaUsesDeterministicSpawnTag(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	const delta = "spawnery/delta:sp1"
	f.Images[delta] = ImageInfo{ID: "sha256:delta", Layers: 6}
	f.ImageArchives = map[string][]byte{delta: []byte("tar-stream-not-gzip")}

	var buf bytes.Buffer
	if err := b.ExportDelta(context.Background(), "sp1", &buf); err != nil {
		t.Fatalf("ExportDelta: %v", err)
	}
	if len(f.ExportedImages) != 1 || f.ExportedImages[0] != delta {
		t.Fatalf("ExportedImages = %v, want [%s]", f.ExportedImages, delta)
	}
	if got := buf.Bytes(); !bytes.Equal(got, []byte("tar-stream-not-gzip")) {
		t.Fatalf("exported bytes = %q", got)
	}
	if bytes.HasPrefix(buf.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("ExportDelta must feed Kopia an uncompressed tar stream, not gzip")
	}
}

func TestExportDeltaRejectsMissingSpawnTag(t *testing.T) {
	b := NewDockerPodBackend(NewFake(), "", "smoke")
	if err := b.ExportDelta(context.Background(), "missing", &bytes.Buffer{}); err == nil {
		t.Fatal("ExportDelta must reject a missing deterministic delta tag")
	}
}

func TestImportDeltaLoadsDeterministicSpawnTag(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	f.Images["base@sha256:abc"] = ImageInfo{ID: "sha256:base", Layers: 5}

	ref, err := b.ImportDelta(context.Background(), "sp1", "base@sha256:abc", bytes.NewReader([]byte("spawnery/delta:sp1\npayload")))
	if err != nil {
		t.Fatalf("ImportDelta: %v", err)
	}
	if ref != "spawnery/delta:sp1" {
		t.Fatalf("ImportDelta ref = %q, want spawnery/delta:sp1", ref)
	}
	if len(f.ImportedImages) != 1 || f.ImportedImages[0] != "spawnery/delta:sp1" {
		t.Fatalf("ImportedImages = %v, want [spawnery/delta:sp1]", f.ImportedImages)
	}
	if _, ok, err := f.InspectImage(context.Background(), "spawnery/delta:sp1"); err != nil || !ok {
		t.Fatalf("imported delta tag not inspectable: ok=%v err=%v", ok, err)
	}
}

// TestDockerPodBackendPauseUnpause verifies that Pause/Unpause call PauseContainer/UnpauseContainer
// on the AGENT container only.
func TestDockerPodBackendPauseUnpause(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()

	// StartPod (fake-1 = sidecar) + StartAgent (fake-2 = agent).
	h, err := b.StartPod(ctx, PodSpec{ID: "sp1", SidecarImage: "s", Resources: Resources{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.StartAgent(ctx, h, AgentSpec{Image: "a", Resources: Resources{}}); err != nil {
		t.Fatal(err)
	}
	// h.AgentID == "fake-2"; sidecar is "fake-1".

	if err := b.Pause(ctx, h); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if len(f.Paused) != 1 || f.Paused[0] != "fake-2" {
		t.Errorf("Paused = %v, want [fake-2]", f.Paused)
	}
	// Sidecar must NOT be paused.
	for _, id := range f.Paused {
		if id == "fake-1" {
			t.Error("sidecar must not be paused")
		}
	}

	if err := b.Unpause(ctx, h); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if len(f.Unpaused) != 1 || f.Unpaused[0] != "fake-2" {
		t.Errorf("Unpaused = %v, want [fake-2]", f.Unpaused)
	}
}

// TestDockerPodBackendPauseRejectsEmptyAgentID verifies that Pause/Unpause return an error
// and do not call the runtime when AgentID is empty.
func TestDockerPodBackendPauseRejectsEmptyAgentID(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(f, "", "smoke")
	ctx := context.Background()
	h := &PodHandle{} // no AgentID

	if err := b.Pause(ctx, h); err == nil {
		t.Fatal("Pause must error when AgentID is empty")
	}
	if len(f.Paused) != 0 {
		t.Errorf("PauseContainer must not be called when AgentID is empty; Paused=%v", f.Paused)
	}

	if err := b.Unpause(ctx, h); err == nil {
		t.Fatal("Unpause must error when AgentID is empty")
	}
	if len(f.Unpaused) != 0 {
		t.Errorf("UnpauseContainer must not be called when AgentID is empty; Unpaused=%v", f.Unpaused)
	}
}

// errOnPause wraps FakeRuntime and returns an error from PauseContainer.
type errOnPause struct{ *FakeRuntime }

func (r errOnPause) PauseContainer(_ context.Context, _ string) error {
	return errors.New("pause failed")
}

// TestDockerPodBackendPauseErrorPropagation verifies that ContainerRuntime errors are surfaced.
func TestDockerPodBackendPauseErrorPropagation(t *testing.T) {
	f := NewFake()
	b := NewDockerPodBackend(errOnPause{f}, "", "smoke")
	ctx := context.Background()

	h, err := b.StartPod(ctx, PodSpec{ID: "sp1", SidecarImage: "s", Resources: Resources{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.StartAgent(ctx, h, AgentSpec{Image: "a", Resources: Resources{}}); err != nil {
		t.Fatal(err)
	}

	if err := b.Pause(ctx, h); err == nil {
		t.Fatal("Pause must propagate ContainerRuntime error")
	}
}

// A2b: The moby#47065 layer-count guard catches a zero-layer commit on a chained delta
// (second suspend: the agent was launched from a prior delta image, not the original base).
// Without this fix, baseLayers = original base (5), committed = delta+0 = 6 > 5 → no error (BUG).
// With the fix, BaseImageRef is the launch image (delta = 6), committed = 6 ≤ 6 → error (CORRECT).
func TestCaptureDeltaLayerGuardChained(t *testing.T) {
	f := NewFake()
	const deltaRef = "spawnery/delta:sp1" // first delta = the launch image for the second suspend
	f.Images[deltaRef] = ImageInfo{ID: "sha256:delta1", Layers: 6}
	// The agent was resumed from the delta image (not the original base).
	f.Started = append(f.Started, ContainerSpec{Image: deltaRef})
	// Force CommitContainer to produce 6 layers (= launch image layers, i.e. zero real writes).
	f.CommitLayers = 6

	b := NewDockerPodBackend(f, "", "smoke")
	// BaseImageRef is the launch image (the first delta), not the original base.
	h := &PodHandle{SpawnID: "sp1", AgentID: "ag", BaseImageRef: deltaRef}

	_, err := b.CaptureDelta(context.Background(), h)
	if err == nil {
		t.Fatal("CaptureDelta must error when committed layers <= launch image layers (chained delta zero-layer guard)")
	}
	if !strings.Contains(err.Error(), "guard") && !strings.Contains(err.Error(), "47065") {
		t.Fatalf("error should mention the guard: %v", err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}
