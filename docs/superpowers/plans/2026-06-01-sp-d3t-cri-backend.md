# CRI Pod Backend Lifecycle (sp-d3t) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a `runtime.PodBackend` over the containerd CRI gRPC API (`k8s.io/cri-api`), so the runsc path can run the agent+sidecar as a one-sandbox pod — driven and tested hermetically against an in-process fake CRI server.

**Architecture:** A new `internal/runtime/cri` package: a thin `Client` (dials a CRI endpoint, holds the `RuntimeService` + `ImageService` gRPC clients) and `CRIPodBackend` implementing the two-phase `PodBackend` (`StartPod` = `RunPodSandbox` + sidecar `CreateContainer`/`StartContainer` + `PodSandboxStatus`→IP/netns; `StartAgent` = agent container; `Stop` = `StopPodSandbox`/`RemovePodSandbox`). The pod IP comes from `PodSandboxStatus.Status.Network.Ip`; the netns path is derived from the verbose `Info["info"]` JSON `pid`. Nothing wires this into the `Manager` yet — slice 5 selects the backend by `CONTAINER_RUNTIME`.

**Tech Stack:** Go 1.25, `k8s.io/cri-api v0.32.1` (already added; CRI **v1** API), `google.golang.org/grpc` + `google.golang.org/grpc/test/bufconn` for the hermetic fake server.

**Scope:** Node only, new `internal/runtime/cri` package + a small `PodHandle.SandboxID` / `Spawn.SandboxID` plumbing change. No CP/proto/manifest/web changes. **This sandbox has no containerd** — every test here is hermetic against a fake CRI gRPC server; real-containerd validation is slice 5 (`sp-ghx`). Commits use `--no-verify` (the `.beads` export hook dirties commits).

---

## CRI API reference (verified against `k8s.io/cri-api@v0.32.1/pkg/apis/runtime/v1`)

Import alias used throughout: `runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"`.

- **Service clients:** `runtimeapi.NewRuntimeServiceClient(conn)` / `NewImageServiceClient(conn)`. **Servers (for the fake):** embed `runtimeapi.UnimplementedRuntimeServiceServer` / `UnimplementedImageServiceServer`; register with `runtimeapi.RegisterRuntimeServiceServer(s, srv)` / `RegisterImageServiceServer(s, srv)`.
- **RunPodSandbox**: `RunPodSandboxRequest{Config *PodSandboxConfig, RuntimeHandler string}` → `RunPodSandboxResponse{PodSandboxId string}`.
- **PodSandboxConfig**: `{Metadata *PodSandboxMetadata{Name,Uid,Namespace string; Attempt uint32}, Linux *LinuxPodSandboxConfig, ...}`.
- **CreateContainer**: `CreateContainerRequest{PodSandboxId string, Config *ContainerConfig, SandboxConfig *PodSandboxConfig}` → `CreateContainerResponse{ContainerId string}`.
- **ContainerConfig**: `{Metadata *ContainerMetadata{Name string}, Image *ImageSpec{Image string}, Command/Args []string, Envs []*KeyValue{Key,Value string}, Mounts []*Mount{ContainerPath,HostPath string; Readonly bool}, Linux *LinuxContainerConfig}`.
- **LinuxContainerConfig**: `{Resources *LinuxContainerResources, SecurityContext *LinuxContainerSecurityContext}`.
- **LinuxContainerResources**: `{MemoryLimitInBytes int64, CpuPeriod int64, CpuQuota int64, Unified map[string]string, ...}` — **there is NO pids field; use `Unified["pids.max"]`** (cgroup-v2).
- **LinuxContainerSecurityContext**: `{Capabilities *Capability{DropCapabilities []string}, ReadonlyRootfs bool, ...}`.
- **StartContainer/StopContainer**: `StartContainerRequest{ContainerId string}`; `StopContainerRequest{ContainerId string, Timeout int64}`.
- **PodSandboxStatus**: `PodSandboxStatusRequest{PodSandboxId string, Verbose bool}` → `PodSandboxStatusResponse{Status *PodSandboxStatus, Info map[string]string}`. `Status.Network.Ip string` = pod IP. `Info["info"]` = a JSON blob containing the sandbox `pid` (containerd) — parse `{"pid": <int>}` and the netns is `/proc/<pid>/ns/net`.
- **StopPodSandbox/RemovePodSandbox**: `{PodSandboxId string}`.
- **Status** (readiness): `StatusRequest{}` → `StatusResponse{Status *RuntimeStatus{Conditions []*RuntimeCondition{Type string, Status bool, Reason string}}}`. Condition types `"RuntimeReady"` / `"NetworkReady"`.
- **ImageStatus**: `ImageStatusRequest{Image *ImageSpec, Verbose bool}` → `ImageStatusResponse{Image *Image}` (nil `Image` = not present). **PullImage**: `PullImageRequest{Image *ImageSpec}`.

---

## File Structure

**New files:**
- `internal/runtime/cri/client.go` — `Client` (dial + the two gRPC service clients).
- `internal/runtime/cri/backend.go` — `CRIPodBackend` implementing `runtime.PodBackend`, plus the small mapping helpers.
- `internal/runtime/cri/fakecri_test.go` — the in-process fake CRI gRPC server (test helper) + a bufconn dialer.
- `internal/runtime/cri/backend_test.go` — hermetic lifecycle tests driving `CRIPodBackend` against the fake.

**Modified files:**
- `internal/runtime/pod.go` — add `SandboxID string` to `PodHandle`.
- `internal/spawnlet/store.go` — add `SandboxID string` to `Spawn`.
- `internal/spawnlet/manager.go` — store `h.SandboxID` on create; pass it back in `Stop`'s handle.

---

## Task 1: SandboxID plumbing (PodHandle + Spawn + Manager)

The CRI backend needs the sandbox id for teardown. Thread it through the generic `PodHandle`, the persisted `Spawn`, and the `Manager`. The Docker backend leaves it empty (additive — existing tests stay green).

**Files:**
- Modify: `internal/runtime/pod.go`
- Modify: `internal/spawnlet/store.go`
- Modify: `internal/spawnlet/manager.go`
- Test: `internal/spawnlet/manager_sandbox_test.go` (new)

- [ ] **Step 1: Write the failing test**

This white-box test (package `spawnlet`) injects a fake `PodBackend` directly onto `m.pod` (the Manager holds an unexported `pod runtime.PodBackend` field) to verify the `SandboxID` round-trips create→store→stop.

Create `internal/spawnlet/manager_sandbox_test.go`:

```go
package spawnlet

import (
	"context"
	"testing"

	"spawnery/internal/runtime"
)

// fakePodBackend records Stop's handle and returns a sandbox-bearing handle from StartPod.
type fakePodBackend struct{ stopped *runtime.PodHandle }

func (f *fakePodBackend) Ping(context.Context) error      { return nil }
func (f *fakePodBackend) Preflight(context.Context) error { return nil }
func (f *fakePodBackend) StartPod(_ context.Context, _ runtime.PodSpec) (*runtime.PodHandle, error) {
	return &runtime.PodHandle{PodIP: "10.0.0.5", NetnsPath: "/proc/7/ns/net", SidecarID: "sc", SandboxID: "sandbox-x"}, nil
}
func (f *fakePodBackend) StartAgent(_ context.Context, h *runtime.PodHandle, _ runtime.AgentSpec) error {
	h.AgentID = "ag"
	return nil
}
func (f *fakePodBackend) Stop(_ context.Context, h *runtime.PodHandle) error { f.stopped = h; return nil }

func TestManagerThreadsSandboxID(t *testing.T) {
	m := NewManager(runtime.NewFake(), ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	fb := &fakePodBackend{}
	m.pod = fb // white-box: replace the Docker backend with the fake

	sp, err := m.Create(context.Background(), "spx", "../../examples/secret-app", "model")
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/spawnlet/ -run TestManagerThreadsSandboxID -v`
Expected: FAIL — `sp.SandboxID` undefined (field doesn't exist yet) / compile error.

- [ ] **Step 3: Add `SandboxID` to `PodHandle`**

In `internal/runtime/pod.go`, add the field to `PodHandle` (after `AgentID`):

```go
	SandboxID string // CRI backend: the pod sandbox id (Docker backend leaves empty)
```

- [ ] **Step 4: Add `SandboxID` to `Spawn`**

In `internal/spawnlet/store.go`, add to the `Spawn` struct (after `NetnsPath`, before `Status`):

```go
	SandboxID string // CRI backend: the pod sandbox id (for teardown); empty for Docker
```

- [ ] **Step 5: Thread it through the Manager**

In `internal/spawnlet/manager.go`:
- In `Create`, add `SandboxID: h.SandboxID,` to the `Spawn{...}` literal (after `NetnsPath: h.NetnsPath,`).
- In `Stop`, add `SandboxID: sp.SandboxID` to the `runtime.PodHandle{...}` passed to `m.pod.Stop` (so it becomes `&runtime.PodHandle{SidecarID: sp.SidecarID, AgentID: sp.AgentID, SandboxID: sp.SandboxID}`).

- [ ] **Step 6: Run the test + the full spawnlet suite**

Run: `go test ./internal/spawnlet/ -race -count=1`
Expected: PASS — the new test passes and all existing tests stay green (the Docker backend leaves `SandboxID` empty, which nothing asserts against).

- [ ] **Step 7: Build + commit**

```bash
go build ./...
git add internal/runtime/pod.go internal/spawnlet/store.go internal/spawnlet/manager.go internal/spawnlet/manager_sandbox_test.go
git commit --no-verify -m "feat(runtime): thread pod SandboxID through PodHandle/Spawn/Manager (sp-d3t)"
```

---

## Task 2: CRI client + fake CRI server + readiness

Stand up the gRPC plumbing: the `Client` wrapper, the in-process fake CRI server over bufconn, and `Ping`/`Preflight` (runtime readiness) round-tripping through it.

**Files:**
- Create: `internal/runtime/cri/client.go`
- Create: `internal/runtime/cri/fakecri_test.go`
- Create: `internal/runtime/cri/backend.go` (the struct + Ping/Preflight only in this task)
- Test: `internal/runtime/cri/backend_test.go`

- [ ] **Step 1: Write the `Client`**

Create `internal/runtime/cri/client.go`:

```go
// Package cri implements a runtime.PodBackend over the containerd CRI gRPC API.
package cri

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Client wraps the CRI RuntimeService + ImageService gRPC clients on one connection.
type Client struct {
	conn    *grpc.ClientConn
	runtime runtimeapi.RuntimeServiceClient
	image   runtimeapi.ImageServiceClient
}

// NewClient builds a Client from an existing gRPC connection (used by tests with bufconn).
func NewClient(conn *grpc.ClientConn) *Client {
	return &Client{
		conn:    conn,
		runtime: runtimeapi.NewRuntimeServiceClient(conn),
		image:   runtimeapi.NewImageServiceClient(conn),
	}
}

// Dial connects to a CRI endpoint, e.g. "unix:///run/containerd/containerd.sock".
func Dial(endpoint string) (*Client, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cri dial %s: %w", endpoint, err)
	}
	return NewClient(conn), nil
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }
```

- [ ] **Step 2: Write the fake CRI server (test helper)**

Create `internal/runtime/cri/fakecri_test.go`. It embeds the Unimplemented servers, records calls, returns canned responses, and serves over bufconn. (Later tasks extend `containers`/image behavior; this task needs `Status` + the wiring.)

```go
package cri

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeCRI is an in-process CRI RuntimeService + ImageService for hermetic tests.
type fakeCRI struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer

	mu sync.Mutex

	// readiness reported by Status.
	runtimeReady bool
	networkReady bool

	// canned StartPod responses.
	sandboxID string
	podIP     string
	infoPid   int // -> Info["info"] {"pid":...}

	// image presence: images already pulled (ImageStatus returns non-nil).
	present map[string]bool

	// recorded calls.
	createdNames []string            // container Metadata.Name in CreateContainer order
	created      []*runtimeapi.ContainerConfig
	createSandbox []string           // PodSandboxId per CreateContainer
	started      []string            // StartContainer ids
	stopped      []string            // StopContainer ids
	pulled       []string            // PullImage images
	stopSandbox  []string
	removeSandbox []string
	nextID       int
}

func (f *fakeCRI) Status(_ context.Context, _ *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	return &runtimeapi.StatusResponse{Status: &runtimeapi.RuntimeStatus{Conditions: []*runtimeapi.RuntimeCondition{
		{Type: "RuntimeReady", Status: f.runtimeReady},
		{Type: "NetworkReady", Status: f.networkReady},
	}}}, nil
}

// newFakeCRI starts the fake over bufconn and returns a connected *Client + the fake for assertions.
func newFakeCRI(t *testing.T) (*Client, *fakeCRI) {
	t.Helper()
	f := &fakeCRI{
		runtimeReady: true, networkReady: true,
		sandboxID: "sandbox-1", podIP: "10.244.0.7", infoPid: 4242,
		present: map[string]bool{},
	}
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(s, f)
	runtimeapi.RegisterImageServiceServer(s, f)
	go func() { _ = s.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); s.Stop() })
	return NewClient(conn), f
}
```

- [ ] **Step 3: Write the backend struct + Ping/Preflight**

Create `internal/runtime/cri/backend.go`:

```go
package cri

import (
	"context"
	"fmt"
	"sync"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"spawnery/internal/runtime"
)

// CRIPodBackend runs a spawn pod as one CRI sandbox (handler=runsc) with two containers
// (sidecar + agent) sharing the pod network namespace. Implements runtime.PodBackend.
type CRIPodBackend struct {
	c              *Client
	runtimeHandler string // e.g. "runsc"

	mu          sync.Mutex
	sandboxCfgs map[string]*runtimeapi.PodSandboxConfig // sandboxID -> config (CreateContainer needs it)
}

// NewCRIPodBackend builds a backend over a Client, running pods under runtimeHandler.
func NewCRIPodBackend(c *Client, runtimeHandler string) *CRIPodBackend {
	return &CRIPodBackend{c: c, runtimeHandler: runtimeHandler, sandboxCfgs: map[string]*runtimeapi.PodSandboxConfig{}}
}

// Ping checks the CRI runtime is reachable.
func (b *CRIPodBackend) Ping(ctx context.Context) error {
	_, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	return err
}

// Preflight asserts the runtime + network are ready (caught at startup, not first spawn).
func (b *CRIPodBackend) Preflight(ctx context.Context) error {
	resp, err := b.c.runtime.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return fmt.Errorf("cri status: %w", err)
	}
	for _, cond := range resp.GetStatus().GetConditions() {
		if (cond.Type == "RuntimeReady" || cond.Type == "NetworkReady") && !cond.Status {
			return fmt.Errorf("cri not ready: %s (%s)", cond.Type, cond.Reason)
		}
	}
	return nil
}
```

- [ ] **Step 4: Write the readiness test**

Create `internal/runtime/cri/backend_test.go`:

```go
package cri

import (
	"context"
	"testing"
)

func TestPingAndPreflight(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := b.Preflight(ctx); err != nil {
		t.Fatalf("Preflight (ready): %v", err)
	}

	f.networkReady = false
	if err := b.Preflight(ctx); err == nil {
		t.Fatal("Preflight must fail when NetworkReady is false")
	}
}
```

- [ ] **Step 5: Resolve deps, run, build**

Run: `go mod tidy` (promotes `k8s.io/cri-api` to a direct require; pulls grpc bufconn into the test graph).
Run: `go test ./internal/runtime/cri/ -race -v`
Expected: PASS.
Run: `go build ./... && go vet ./internal/runtime/cri/`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/cri/client.go internal/runtime/cri/backend.go internal/runtime/cri/fakecri_test.go internal/runtime/cri/backend_test.go go.mod go.sum
git commit --no-verify -m "feat(cri): client + fake CRI server + Ping/Preflight (sp-d3t)"
```

---

## Task 3: StartPod (sandbox + sidecar + IP/netns + image pull)

Implement `StartPod` and its mapping helpers; extend the fake to handle sandbox + container + status + image calls.

**Files:**
- Modify: `internal/runtime/cri/backend.go`
- Modify: `internal/runtime/cri/fakecri_test.go`
- Test: `internal/runtime/cri/backend_test.go`

- [ ] **Step 1: Extend the fake CRI server with the StartPod-path methods**

Append to `internal/runtime/cri/fakecri_test.go`:

```go
import "encoding/json" // add to the existing import block if not present

func (f *fakeCRI) nextContainerID() string {
	f.nextID++
	return fmt.Sprintf("ctr-%d", f.nextID) // add "fmt" to imports
}

func (f *fakeCRI) RunPodSandbox(_ context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: f.sandboxID}, nil
}

func (f *fakeCRI) PodSandboxStatus(_ context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	info, _ := json.Marshal(struct {
		Pid int `json:"pid"`
	}{Pid: f.infoPid})
	return &runtimeapi.PodSandboxStatusResponse{
		Status: &runtimeapi.PodSandboxStatus{Id: req.PodSandboxId, Network: &runtimeapi.PodSandboxNetworkStatus{Ip: f.podIP}},
		Info:   map[string]string{"info": string(info)},
	}, nil
}

func (f *fakeCRI) CreateContainer(_ context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextContainerID()
	f.created = append(f.created, req.Config)
	f.createdNames = append(f.createdNames, req.Config.GetMetadata().GetName())
	f.createSandbox = append(f.createSandbox, req.PodSandboxId)
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

func (f *fakeCRI) StartContainer(_ context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	f.mu.Lock()
	f.started = append(f.started, req.ContainerId)
	f.mu.Unlock()
	return &runtimeapi.StartContainerResponse{}, nil
}

func (f *fakeCRI) ImageStatus(_ context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present[req.Image.GetImage()] {
		return &runtimeapi.ImageStatusResponse{Image: &runtimeapi.Image{Id: req.Image.GetImage()}}, nil
	}
	return &runtimeapi.ImageStatusResponse{}, nil // not present
}

func (f *fakeCRI) PullImage(_ context.Context, req *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	f.mu.Lock()
	f.pulled = append(f.pulled, req.Image.GetImage())
	f.present[req.Image.GetImage()] = true
	f.mu.Unlock()
	return &runtimeapi.PullImageResponse{ImageRef: req.Image.GetImage()}, nil
}
```

Ensure `fakecri_test.go`'s import block includes `"encoding/json"` and `"fmt"`.

- [ ] **Step 2: Write the StartPod test (failing)**

Append to `internal/runtime/cri/backend_test.go`:

```go
func TestStartPodSandboxSidecarAndHandle(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{ // add the runtime import to backend_test.go
		ID:           "spawn-7",
		SidecarImage: "spawnery/sidecar:dev",
		SidecarEnv:   []string{"OPENROUTER_API_KEY=k", "SIDECAR_ADDR=127.0.0.1:8080"},
		Resources:    runtime.Resources{MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PidsLimit: 128},
		Runtime:      "runsc",
	})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}
	if h.SandboxID != "sandbox-1" {
		t.Fatalf("SandboxID = %q", h.SandboxID)
	}
	if h.PodIP != "10.244.0.7" {
		t.Fatalf("PodIP = %q", h.PodIP)
	}
	if h.NetnsPath != "/proc/4242/ns/net" {
		t.Fatalf("NetnsPath = %q", h.NetnsPath)
	}
	if h.SidecarID != "ctr-1" {
		t.Fatalf("SidecarID = %q", h.SidecarID)
	}
	if h.AgentID != "" {
		t.Fatalf("AgentID must be empty after StartPod, got %q", h.AgentID)
	}
	// Exactly the sidecar container was created+started, in the sandbox, with the image pulled.
	if len(f.created) != 1 || f.createdNames[0] != "sidecar" || f.createSandbox[0] != "sandbox-1" {
		t.Fatalf("sidecar create wrong: names=%v sandbox=%v", f.createdNames, f.createSandbox)
	}
	if len(f.started) != 1 || f.started[0] != "ctr-1" {
		t.Fatalf("started = %v", f.started)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "spawnery/sidecar:dev" {
		t.Fatalf("pulled = %v", f.pulled)
	}
	// Resource mapping: 512MiB mem, 2 cores -> quota=200000/period=100000, pids via Unified.
	sc := f.created[0]
	if sc.Linux.Resources.MemoryLimitInBytes != 512<<20 {
		t.Fatalf("mem = %d", sc.Linux.Resources.MemoryLimitInBytes)
	}
	if sc.Linux.Resources.CpuPeriod != 100000 || sc.Linux.Resources.CpuQuota != 200000 {
		t.Fatalf("cpu period/quota = %d/%d", sc.Linux.Resources.CpuPeriod, sc.Linux.Resources.CpuQuota)
	}
	if sc.Linux.Resources.Unified["pids.max"] != "128" {
		t.Fatalf("pids.max = %q", sc.Linux.Resources.Unified["pids.max"])
	}
}
```

Add `"spawnery/internal/runtime"` to `backend_test.go`'s imports.

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/runtime/cri/ -run TestStartPod -v`
Expected: FAIL — `b.StartPod` undefined.

- [ ] **Step 4: Implement StartPod + helpers**

Append to `internal/runtime/cri/backend.go` (add imports `"encoding/json"`, `"strconv"`, `"strings"`):

```go
// StartPod runs the pod sandbox and starts the (trusted) sidecar, returning a handle with the pod IP
// (for the egress floor) and netns path (for the ACP attach). The agent is not started yet.
func (b *CRIPodBackend) StartPod(ctx context.Context, spec runtime.PodSpec) (*runtime.PodHandle, error) {
	sandboxCfg := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{Name: spec.ID, Uid: spec.ID, Namespace: "spawnery"},
		Linux:    &runtimeapi.LinuxPodSandboxConfig{},
	}
	sb, err := b.c.runtime.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{Config: sandboxCfg, RuntimeHandler: b.runtimeHandler})
	if err != nil {
		return nil, fmt.Errorf("run pod sandbox: %w", err)
	}
	sandboxID := sb.PodSandboxId
	cleanup := func() { b.removeSandbox(context.WithoutCancel(ctx), sandboxID) }

	if err := b.pullImage(ctx, spec.SidecarImage); err != nil {
		cleanup()
		return nil, err
	}
	sidecarID, err := b.createAndStart(ctx, sandboxID, sandboxCfg, &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: "sidecar"},
		Image:    &runtimeapi.ImageSpec{Image: spec.SidecarImage},
		Envs:     toKeyValues(spec.SidecarEnv),
		Linux:    linuxContainer(spec.Resources, false, false),
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("sidecar: %w", err)
	}

	st, err := b.c.runtime.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("pod sandbox status: %w", err)
	}
	ip := st.GetStatus().GetNetwork().GetIp()
	if ip == "" {
		cleanup()
		return nil, fmt.Errorf("pod sandbox %s has no IP", sandboxID)
	}
	netns, err := netnsPathFromInfo(st.Info)
	if err != nil {
		cleanup()
		return nil, err
	}

	b.mu.Lock()
	b.sandboxCfgs[sandboxID] = sandboxCfg
	b.mu.Unlock()
	return &runtime.PodHandle{PodIP: ip, NetnsPath: netns, SidecarID: sidecarID, SandboxID: sandboxID}, nil
}

func (b *CRIPodBackend) createAndStart(ctx context.Context, sandboxID string, sandboxCfg *runtimeapi.PodSandboxConfig, cfg *runtimeapi.ContainerConfig) (string, error) {
	cr, err := b.c.runtime.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{PodSandboxId: sandboxID, Config: cfg, SandboxConfig: sandboxCfg})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	if _, err := b.c.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: cr.ContainerId}); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return cr.ContainerId, nil
}

// pullImage pulls the image if not already present in the CRI (k8s.io) image store.
func (b *CRIPodBackend) pullImage(ctx context.Context, image string) error {
	spec := &runtimeapi.ImageSpec{Image: image}
	if st, err := b.c.image.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: spec}); err == nil && st.GetImage() != nil {
		return nil
	}
	if _, err := b.c.image.PullImage(ctx, &runtimeapi.PullImageRequest{Image: spec}); err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	return nil
}

func (b *CRIPodBackend) removeSandbox(ctx context.Context, sandboxID string) {
	_, _ = b.c.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: sandboxID})
	_, _ = b.c.runtime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: sandboxID})
	b.mu.Lock()
	delete(b.sandboxCfgs, sandboxID)
	b.mu.Unlock()
}

// netnsPathFromInfo extracts the sandbox pid from CRI verbose Info and returns its net ns path.
func netnsPathFromInfo(info map[string]string) (string, error) {
	raw, ok := info["info"]
	if !ok {
		return "", fmt.Errorf("pod sandbox status missing verbose info")
	}
	var v struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", fmt.Errorf("parse sandbox info: %w", err)
	}
	if v.Pid == 0 {
		return "", fmt.Errorf("pod sandbox info has no pid")
	}
	return fmt.Sprintf("/proc/%d/ns/net", v.Pid), nil
}

func toKeyValues(env []string) []*runtimeapi.KeyValue {
	out := make([]*runtimeapi.KeyValue, 0, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out = append(out, &runtimeapi.KeyValue{Key: k, Value: v})
	}
	return out
}

func toCRIMounts(ms []runtime.Mount) []*runtimeapi.Mount {
	out := make([]*runtimeapi.Mount, 0, len(ms))
	for _, m := range ms {
		out = append(out, &runtimeapi.Mount{ContainerPath: m.ContainerPath, HostPath: m.HostPath, Readonly: m.ReadOnly})
	}
	return out
}

// linuxContainer maps our Resources + hardening flags to the CRI LinuxContainerConfig. Pids has no
// dedicated CRI field, so it goes through the cgroup-v2 Unified map ("pids.max").
func linuxContainer(res runtime.Resources, dropCaps, roRootfs bool) *runtimeapi.LinuxContainerConfig {
	r := &runtimeapi.LinuxContainerResources{}
	if res.MemoryBytes > 0 {
		r.MemoryLimitInBytes = res.MemoryBytes
	}
	if res.NanoCPUs > 0 {
		const period = 100000 // 100ms, in microseconds
		r.CpuPeriod = period
		r.CpuQuota = res.NanoCPUs * period / 1_000_000_000
	}
	if res.PidsLimit > 0 {
		r.Unified = map[string]string{"pids.max": strconv.FormatInt(res.PidsLimit, 10)}
	}
	lc := &runtimeapi.LinuxContainerConfig{Resources: r}
	if dropCaps || roRootfs {
		lc.SecurityContext = &runtimeapi.LinuxContainerSecurityContext{}
		if dropCaps {
			lc.SecurityContext.Capabilities = &runtimeapi.Capability{DropCapabilities: []string{"ALL"}}
		}
		if roRootfs {
			lc.SecurityContext.ReadonlyRootfs = true
		}
	}
	return lc
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/runtime/cri/ -race -v`
Expected: PASS (readiness + StartPod tests).

- [ ] **Step 6: Build + vet + commit**

```bash
go build ./... && go vet ./internal/runtime/cri/
git add internal/runtime/cri/backend.go internal/runtime/cri/fakecri_test.go internal/runtime/cri/backend_test.go
git commit --no-verify -m "feat(cri): StartPod — sandbox + sidecar + IP/netns + image pull (sp-d3t)"
```

---

## Task 4: StartAgent + Stop + full lifecycle

Complete the `PodBackend`: `StartAgent` (agent container into the sandbox) and `Stop` (containers + sandbox teardown), with a full create→stop lifecycle test asserting the recorded CRI call sequence.

**Files:**
- Modify: `internal/runtime/cri/backend.go`
- Modify: `internal/runtime/cri/fakecri_test.go`
- Test: `internal/runtime/cri/backend_test.go`

- [ ] **Step 1: Extend the fake with stop/remove recording**

Append to `internal/runtime/cri/fakecri_test.go`:

```go
func (f *fakeCRI) StopContainer(_ context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	f.mu.Lock()
	f.stopped = append(f.stopped, req.ContainerId)
	f.mu.Unlock()
	return &runtimeapi.StopContainerResponse{}, nil
}

func (f *fakeCRI) StopPodSandbox(_ context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	f.stopSandbox = append(f.stopSandbox, req.PodSandboxId)
	f.mu.Unlock()
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *fakeCRI) RemovePodSandbox(_ context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	f.mu.Lock()
	f.removeSandbox = append(f.removeSandbox, req.PodSandboxId)
	f.mu.Unlock()
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}
```

- [ ] **Step 2: Write the lifecycle test (failing)**

Append to `internal/runtime/cri/backend_test.go`:

```go
func TestStartAgentAndStopLifecycle(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	h, err := b.StartPod(ctx, runtime.PodSpec{ID: "spawn-7", SidecarImage: "sidecar:dev", Resources: runtime.Resources{MemoryBytes: 1 << 20}})
	if err != nil {
		t.Fatalf("StartPod: %v", err)
	}

	err = b.StartAgent(ctx, h, runtime.AgentSpec{
		Image:          "goose:dev",
		Env:            []string{"SPAWN_MODEL=m"},
		Mounts:         []runtime.Mount{{HostPath: "/h", ContainerPath: "/app", ReadOnly: true}},
		Resources:      runtime.Resources{MemoryBytes: 1 << 20},
		DropAllCaps:    true,
		ReadonlyRootfs: true,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if h.AgentID != "ctr-2" {
		t.Fatalf("AgentID = %q", h.AgentID)
	}
	// Agent container created in the same sandbox, with hardening + the mount mapped.
	if len(f.created) != 2 || f.createdNames[1] != "agent" || f.createSandbox[1] != "sandbox-1" {
		t.Fatalf("agent create wrong: names=%v", f.createdNames)
	}
	ag := f.created[1]
	if ag.Linux.SecurityContext == nil || len(ag.Linux.SecurityContext.Capabilities.DropCapabilities) != 1 ||
		ag.Linux.SecurityContext.Capabilities.DropCapabilities[0] != "ALL" || !ag.Linux.SecurityContext.ReadonlyRootfs {
		t.Fatalf("agent hardening wrong: %+v", ag.Linux.SecurityContext)
	}
	if len(ag.Mounts) != 1 || ag.Mounts[0].HostPath != "/h" || ag.Mounts[0].ContainerPath != "/app" || !ag.Mounts[0].Readonly {
		t.Fatalf("agent mount wrong: %+v", ag.Mounts)
	}

	if err := b.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Both containers stopped (agent first), then the sandbox stopped + removed.
	if len(f.stopped) != 2 || f.stopped[0] != "ctr-2" || f.stopped[1] != "ctr-1" {
		t.Fatalf("stopped order = %v", f.stopped)
	}
	if len(f.stopSandbox) != 1 || f.stopSandbox[0] != "sandbox-1" || len(f.removeSandbox) != 1 || f.removeSandbox[0] != "sandbox-1" {
		t.Fatalf("sandbox teardown wrong: stop=%v remove=%v", f.stopSandbox, f.removeSandbox)
	}
}

func TestStartAgentUnknownSandbox(t *testing.T) {
	c, _ := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	err := b.StartAgent(context.Background(), &runtime.PodHandle{SandboxID: "nope"}, runtime.AgentSpec{Image: "x"})
	if err == nil {
		t.Fatal("StartAgent must error for an unknown sandbox")
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/runtime/cri/ -run 'TestStartAgent|TestStartAgentUnknownSandbox' -v`
Expected: FAIL — `b.StartAgent` / `b.Stop` undefined.

- [ ] **Step 4: Implement StartAgent + Stop**

Append to `internal/runtime/cri/backend.go`:

```go
// StartAgent starts the (untrusted) agent container in the existing pod sandbox.
func (b *CRIPodBackend) StartAgent(ctx context.Context, h *runtime.PodHandle, spec runtime.AgentSpec) error {
	b.mu.Lock()
	sandboxCfg := b.sandboxCfgs[h.SandboxID]
	b.mu.Unlock()
	if sandboxCfg == nil {
		return fmt.Errorf("unknown sandbox %s", h.SandboxID)
	}
	if err := b.pullImage(ctx, spec.Image); err != nil {
		return err
	}
	agentID, err := b.createAndStart(ctx, h.SandboxID, sandboxCfg, &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{Name: "agent"},
		Image:    &runtimeapi.ImageSpec{Image: spec.Image},
		Envs:     toKeyValues(spec.Env),
		Mounts:   toCRIMounts(spec.Mounts),
		Linux:    linuxContainer(spec.Resources, spec.DropAllCaps, spec.ReadonlyRootfs),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// Stop tears down the agent + sidecar, then stops and removes the pod sandbox. Best-effort; empty
// ids are skipped (e.g. agent never started on the fail-closed floor path).
func (b *CRIPodBackend) Stop(ctx context.Context, h *runtime.PodHandle) error {
	ctx = context.WithoutCancel(ctx)
	if h.AgentID != "" {
		_, _ = b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.AgentID})
	}
	if h.SidecarID != "" {
		_, _ = b.c.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: h.SidecarID})
	}
	if h.SandboxID != "" {
		b.removeSandbox(ctx, h.SandboxID)
	}
	return nil
}
```

- [ ] **Step 5: Run the full package suite**

Run: `go test ./internal/runtime/cri/ -race -count=1 -v`
Expected: PASS (readiness, StartPod, StartAgent lifecycle, unknown-sandbox).

- [ ] **Step 6: Confirm the backend satisfies the interface + whole module green**

Add this compile-time assertion at the end of `internal/runtime/cri/backend.go`:

```go
var _ runtime.PodBackend = (*CRIPodBackend)(nil)
```

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: exit 0 / all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/cri/backend.go internal/runtime/cri/fakecri_test.go internal/runtime/cri/backend_test.go
git commit --no-verify -m "feat(cri): StartAgent + Stop + full pod lifecycle (sp-d3t)"
```

---

## Self-Review

**1. Spec coverage (spec §3.2/§3.5/§3.6 + slice 3):**
- `internal/runtime/cri` package with a `cri-api` gRPC client → Tasks 2. ✓
- `RunPodSandbox(handler=runsc)` + `CreateContainer`/`StartContainer` ×2 → Tasks 3 (sidecar) + 4 (agent). ✓
- `PodIP` from `PodSandboxStatus.Network.Ip`; `NetnsPath` from verbose `Info` pid → Task 3. ✓
- Image pull via CRI `ImageService` (k8s.io namespace) → Task 3 `pullImage`. ✓
- Preflight (runtime readiness) → Task 2. (The canary-pod preflight from §3.6 is deferred to the slice-5 host e2e — noted below.) ✓
- `StopPodSandbox`/`RemovePodSandbox` teardown → Task 4. ✓
- Implements `runtime.PodBackend` (compile-time assertion) → Task 4 Step 6. ✓
- Gated behind `CONTAINER_RUNTIME=runsc` — the *selection* is slice 5; this slice builds the backend standalone. ✓
- Hermetic tests against a fake CRI gRPC server → Tasks 2–4. ✓

**2. Placeholder scan:** No TBD/TODO; every code step is complete; every run step has an exact command + expected result.

**3. Type consistency:** `CRIPodBackend`/`NewCRIPodBackend(c, runtimeHandler)`, `Client`/`NewClient`/`Dial`, and the helpers (`createAndStart`/`pullImage`/`removeSandbox`/`netnsPathFromInfo`/`toKeyValues`/`toCRIMounts`/`linuxContainer`) are introduced in Task 2/3 and used consistently in Task 4. `PodHandle.SandboxID` (Task 1) is set by `StartPod` (Task 3) and read by `Stop` (Task 4) + the Manager (Task 1). The fake's recorded fields (`created`/`createdNames`/`createSandbox`/`started`/`stopped`/`pulled`/`stopSandbox`/`removeSandbox`) are declared in Task 2 and populated across Tasks 3–4. `ctr-N` ids and `sandbox-1` match the fake's canned values used in assertions.

**4. Deliberately deferred (not this slice):** the **canary-pod** preflight and all **real-containerd** behavior (actual runsc sandbox, real CNI pod IP, real verbose-info pid shape, real cgroup enforcement, the `docker save | ctr -n k8s.io import` image bridge) are validated on a privileged host in **slice 5 (`sp-ghx`)**; this slice proves the gRPC control flow against a fake. The CRI **egress floor** (`SPAWNLET-EGRESS`) is **slice 4 (`sp-cx4`)**. Backend *selection* by `CONTAINER_RUNTIME` is **slice 5**. Follow-up `sp-mcb` (empty-IP tolerance) applies to the Docker backend, not this one (CRI always requires a pod IP).
