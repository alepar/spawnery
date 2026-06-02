# PodBackend Seam + Docker Backend (sp-0wd) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract a two-phase `runtime.PodBackend` seam and reimplement the Docker/runc pod path behind it, so the CRI backend (slice 3) can slot in alongside without touching the `Manager`.

**Architecture:** A new `PodBackend` interface (`StartPod` → floor → `StartAgent` → `Stop`, plus `Ping`/`Preflight`) with one implementation, `DockerPodBackend`, that wraps the existing per-container `runtime.ContainerRuntime`. The `Manager` keeps everything shared (manifest parse, mount prep, the egress floor, the store) and drives the pod lifecycle through the backend. `NewManager` keeps its `(rt runtime.ContainerRuntime, cfg)` signature and wraps `rt` in a `DockerPodBackend` internally, so this is a pure refactor: **every existing test stays green unmodified.**

**Tech Stack:** Go 1.25, the existing `internal/runtime` package, `FakeRuntime` for hermetic tests.

**Scope:** Node only. No CP/proto/manifest/web changes. No behavior change (one deliberate, minor tightening noted in Task 1). Commits use `--no-verify` (the `.beads` export hook dirties commits). Spec: `docs/superpowers/specs/2026-06-01-runsc-cri-pod-backend-design.md` §3.1/§3.2 + slice 2 in §5. This sandbox runs hermetic Go tests (no docker/root needed for any step here).

---

## File Structure

**New files:**
- `internal/runtime/pod.go` — the `PodBackend` interface + `PodSpec`/`AgentSpec`/`Resources`/`PodHandle` value types. One responsibility: the backend contract.
- `internal/runtime/docker_pod.go` — `DockerPodBackend`, implementing `PodBackend` over a `ContainerRuntime` (the runc path: sidecar owns the netns, agent joins via `NetnsOf`).
- `internal/runtime/docker_pod_test.go` — hermetic tests for `DockerPodBackend` via `FakeRuntime`.

**Modified files:**
- `internal/spawnlet/manager.go` — `Manager` holds a `runtime.PodBackend` (built in `NewManager`); `Create` drives `StartPod` → floor → `StartAgent`; `Stop` and `PreflightRuntime` delegate to the backend. Remove the now-unused `rt` field + `Runtime()` accessor.

**Unchanged (verified):** `cmd/spawnlet/main.go` (still calls `NewManager(rt, cfg)` + `mgr.PreflightRuntime`), and all `internal/spawnlet/*_test.go` (they pass `runtime.NewFake()` to `NewManager` and inspect the same fake).

---

## Task 1: PodBackend interface + DockerPodBackend

Build the new backend in the `runtime` package. It is unused by the `Manager` until Task 2 — this task delivers and tests it standalone.

**Files:**
- Create: `internal/runtime/pod.go`
- Create: `internal/runtime/docker_pod.go`
- Test: `internal/runtime/docker_pod_test.go`

- [ ] **Step 1: Write the failing test**

First, check the test package convention: run `head -1 internal/runtime/docker_test.go`. Use the SAME `package` line for the new test (it is `package runtime` — white-box — so `FakeRuntime` and the new types are directly accessible). Create `internal/runtime/docker_pod_test.go`:

```go
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
	// Handle is filled from the fake (ContainerPID=4242, ContainerIP=172.17.0.99, first id=fake-1).
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
	// Sidecar spec is the only thing started so far, with the resources + runtime mapped through.
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
	// After StartPod but before StartAgent (the fail-closed floor path), Stop must stop only the
	// sidecar and not call StopContainer("").
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

func TestDockerPodBackendPreflight(t *testing.T) {
	ctx := context.Background()
	// No configured runtime -> no-op nil.
	if err := NewDockerPodBackend(NewFake(), "", "smoke").Preflight(ctx); err != nil {
		t.Fatalf("empty runtime should preflight nil, got %v", err)
	}
	// Healthy runtime -> nil.
	if err := NewDockerPodBackend(NewFake(), "runsc", "smoke").Preflight(ctx); err != nil {
		t.Fatalf("healthy runtime should preflight nil, got %v", err)
	}
	// Broken runtime -> error.
	if err := NewDockerPodBackend(errOnRuntime{NewFake()}, "runsc", "smoke").Preflight(ctx); err == nil {
		t.Fatal("broken runtime must fail preflight")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runtime/ -run TestDockerPodBackend -v`
Expected: FAIL — `undefined: NewDockerPodBackend` / `undefined: PodSpec` etc.

- [ ] **Step 3: Write the interface + value types**

Create `internal/runtime/pod.go`:

```go
package runtime

import "context"

// Resources are the per-container cgroup limits applied to both pod containers.
type Resources struct {
	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
}

// PodSpec describes the pod sandbox + its sidecar container (started by StartPod).
type PodSpec struct {
	ID           string // spawn id
	SidecarImage string
	SidecarEnv   []string
	Resources    Resources
	Runtime      string // OCI runtime; "" = default, e.g. "runsc"
}

// AgentSpec describes the agent container (started by StartAgent into the existing pod).
type AgentSpec struct {
	Image          string
	Env            []string
	Mounts         []Mount
	Resources      Resources
	Runtime        string
	DropAllCaps    bool
	ReadonlyRootfs bool
}

// PodHandle identifies a running pod. PodIP (for the egress floor) and NetnsPath (for the ACP
// attach) are read by the Manager; the *ID fields are backend-specific identifiers the Manager
// persists on the Spawn and hands back to Stop.
type PodHandle struct {
	PodIP     string
	NetnsPath string
	SidecarID string // Docker backend: the sidecar container id (netns owner)
	AgentID   string // Docker backend: the agent container id (set by StartAgent)
}

// PodBackend runs a spawn pod: a sidecar + an agent sharing one network namespace, with the model
// key kept isolated in the sidecar. It is two-phase (StartPod then StartAgent) so the egress floor
// can be applied after the pod IP exists and before the untrusted agent starts.
type PodBackend interface {
	Ping(ctx context.Context) error
	Preflight(ctx context.Context) error
	StartPod(ctx context.Context, spec PodSpec) (*PodHandle, error)
	StartAgent(ctx context.Context, h *PodHandle, spec AgentSpec) error
	Stop(ctx context.Context, h *PodHandle) error
}
```

- [ ] **Step 4: Write the Docker backend**

Create `internal/runtime/docker_pod.go`:

```go
package runtime

import (
	"context"
	"fmt"
)

// DockerPodBackend implements PodBackend over the per-container ContainerRuntime (Docker): the
// sidecar owns the pod network namespace and the agent joins it via NetnsOf. This is the runc path.
type DockerPodBackend struct {
	rt          ContainerRuntime
	runtimeName string // OCI runtime smoke-tested by Preflight ("" = default, skip)
	smokeImage  string // image for the preflight smoke container
}

// NewDockerPodBackend wraps a ContainerRuntime. runtimeName + smokeImage drive Preflight.
func NewDockerPodBackend(rt ContainerRuntime, runtimeName, smokeImage string) *DockerPodBackend {
	return &DockerPodBackend{rt: rt, runtimeName: runtimeName, smokeImage: smokeImage}
}

func (d *DockerPodBackend) Ping(ctx context.Context) error { return d.rt.Ping(ctx) }

// Preflight smoke-runs `true` under the configured runtime so a misconfigured runsc is caught at
// startup, not at first spawn. No-op when no non-default runtime is configured.
func (d *DockerPodBackend) Preflight(ctx context.Context) error {
	if d.runtimeName == "" {
		return nil
	}
	id, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:   d.smokeImage,
		Cmd:     []string{"true"},
		Runtime: d.runtimeName,
	})
	if err != nil {
		return fmt.Errorf("runtime %q preflight: %w", d.runtimeName, err)
	}
	_ = d.rt.StopContainer(context.WithoutCancel(ctx), id)
	return nil
}

// StartPod starts the sidecar (which owns the pod netns) and returns a handle carrying the pod IP
// (for the floor) and netns path (for the ACP attach). The agent is not started yet.
func (d *DockerPodBackend) StartPod(ctx context.Context, spec PodSpec) (*PodHandle, error) {
	sidecarID, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:       spec.SidecarImage,
		Env:         spec.SidecarEnv,
		MemoryBytes: spec.Resources.MemoryBytes,
		NanoCPUs:    spec.Resources.NanoCPUs,
		PidsLimit:   spec.Resources.PidsLimit,
		Runtime:     spec.Runtime,
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	pid, err := d.rt.ContainerPID(ctx, sidecarID)
	if err != nil {
		_ = d.rt.StopContainer(context.WithoutCancel(ctx), sidecarID)
		return nil, fmt.Errorf("sidecar pid: %w", err)
	}
	ip, err := d.rt.ContainerIP(ctx, sidecarID)
	if err != nil {
		_ = d.rt.StopContainer(context.WithoutCancel(ctx), sidecarID)
		return nil, fmt.Errorf("sidecar ip: %w", err)
	}
	return &PodHandle{
		PodIP:     ip,
		NetnsPath: fmt.Sprintf("/proc/%d/ns/net", pid),
		SidecarID: sidecarID,
	}, nil
}

// StartAgent starts the agent container in the sidecar's netns and records its id on the handle.
func (d *DockerPodBackend) StartAgent(ctx context.Context, h *PodHandle, spec AgentSpec) error {
	agentID, err := d.rt.StartContainer(ctx, ContainerSpec{
		Image:          spec.Image,
		NetnsOf:        h.SidecarID,
		Env:            spec.Env,
		Mounts:         spec.Mounts,
		AttachStdio:    true,
		MemoryBytes:    spec.Resources.MemoryBytes,
		NanoCPUs:       spec.Resources.NanoCPUs,
		PidsLimit:      spec.Resources.PidsLimit,
		Runtime:        spec.Runtime,
		DropAllCaps:    spec.DropAllCaps,
		ReadonlyRootfs: spec.ReadonlyRootfs,
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	h.AgentID = agentID
	return nil
}

// Stop tears down the agent then the sidecar. Empty ids (e.g. agent not yet started on the
// fail-closed floor path) are skipped.
func (d *DockerPodBackend) Stop(ctx context.Context, h *PodHandle) error {
	if h.AgentID != "" {
		_ = d.rt.StopContainer(ctx, h.AgentID)
	}
	if h.SidecarID != "" {
		_ = d.rt.StopContainer(ctx, h.SidecarID)
	}
	return nil
}
```

> **Note (deliberate minor tightening):** `StartPod` now fetches the pod IP **unconditionally** (before, the Manager fetched it only when the floor was enforced). For Docker a running container always has a bridge IP, and the Fake returns one, so no test breaks; it makes the handle self-describing for both backends. This is the only behavior delta in the slice.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/runtime/ -run TestDockerPodBackend -race -v`
Expected: PASS (all three tests).

- [ ] **Step 6: Build + vet the package**

Run: `go build ./... && go vet ./internal/runtime/`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/pod.go internal/runtime/docker_pod.go internal/runtime/docker_pod_test.go
git commit --no-verify -m "feat(runtime): two-phase PodBackend seam + DockerPodBackend (sp-0wd)"
```

---

## Task 2: Rewire the Manager onto the PodBackend

Switch `Manager` from the bare `ContainerRuntime` to the `PodBackend`. `NewManager` keeps its signature and wraps the passed `rt` in a `DockerPodBackend`, so **no call site or test changes** — the existing spawnlet test suite is the regression net.

**Files:**
- Modify: `internal/spawnlet/manager.go`

- [ ] **Step 1: Confirm the regression net is green before changing anything**

Run: `go test ./internal/spawnlet/ -race -count=1`
Expected: PASS (baseline). These tests must STILL pass after the rewrite without modification.

- [ ] **Step 2: Swap the Manager's field and constructor wiring**

In `internal/spawnlet/manager.go`, change the `Manager` struct: replace the `rt runtime.ContainerRuntime` field with `pod runtime.PodBackend`. Keep `cfg`, `store`, `backend` (the storage backend), `fw`:

```go
type Manager struct {
	pod     runtime.PodBackend
	cfg     ManagerConfig
	store   *Store
	backend storage.Backend
	fw      firewall.Applier
}
```

In `NewManager` (signature UNCHANGED — still `(rt runtime.ContainerRuntime, cfg ManagerConfig)`), build the Docker backend from `rt`. Replace the final `return &Manager{...}` with:

```go
	return &Manager{
		pod:     runtime.NewDockerPodBackend(rt, cfg.ContainerRuntime, cfg.AgentImage),
		cfg:     cfg,
		store:   NewStore(),
		backend: storage.NewScratch(cfg.DataRoot),
		fw:      firewall.HostFloorApplier{},
	}
```

- [ ] **Step 3: Remove the now-unused `Runtime()` accessor**

`Manager.Runtime()` has zero callers (verified). Delete the method:

```go
func (m *Manager) Runtime() runtime.ContainerRuntime { return m.rt }
```

(Leave `Store()` and `egressEnforced()` as they are.)

- [ ] **Step 4: Rewrite `Manager.Create` to drive the backend**

Replace the body of `Create` **from the resource-limit computation onward** (everything after the mount-prep loop — i.e. from the current `mem := m.cfg.MemLimitMB << 20` line through `return sp, nil`) with:

```go
	res := runtime.Resources{
		MemoryBytes: m.cfg.MemLimitMB << 20,
		NanoCPUs:    int64(m.cfg.CPULimit * 1e9),
		PidsLimit:   m.cfg.PidsLimit,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)

	// Phase 1: sandbox + sidecar (the trusted, key-holding container).
	h, err := m.pod.StartPod(ctx, runtime.PodSpec{
		ID:           id,
		SidecarImage: m.cfg.SidecarImage,
		SidecarEnv: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
		},
		Resources: res,
		Runtime:   m.cfg.ContainerRuntime,
	})
	if err != nil {
		finalizeAll()
		return nil, err
	}

	// Egress floor: applied after the pod IP exists, before the untrusted agent starts (fail-closed).
	var floorIP string
	if m.egressEnforced() {
		if ferr := m.fw.Apply(ctx, firewall.Rules(h.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
		floorIP = h.PodIP
	}

	// Phase 2: the untrusted agent, into the existing pod.
	if err := m.pod.StartAgent(ctx, h, runtime.AgentSpec{
		Image: m.cfg.AgentImage,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
		},
		Mounts:         mounts,
		Resources:      res,
		Runtime:        m.cfg.ContainerRuntime,
		DropAllCaps:    true,
		ReadonlyRootfs: m.cfg.HardenRootfs,
	}); err != nil {
		_ = m.pod.Stop(ctx, h)
		finalizeAll()
		return nil, err
	}

	sp := &Spawn{ID: id, SidecarID: h.SidecarID, AgentID: h.AgentID, MountDirs: mountDirs, FloorIP: floorIP, NetnsPath: h.NetnsPath, Status: "ready"}
	m.store.Put(sp)
	return sp, nil
```

(The lines ABOVE this — `filepath.Abs`, `manifest.Parse`, the `mounts`/`mountDirs`/`finalizeAll` setup, and the mount-prep `for` loop — are UNCHANGED.)

- [ ] **Step 5: Delegate `PreflightRuntime` to the backend**

Replace the entire `PreflightRuntime` method body with a delegation (keep the method name — `cmd/spawnlet` calls `mgr.PreflightRuntime`):

```go
// PreflightRuntime validates a configured non-default container runtime at startup (delegates to the
// backend's smoke check). Callers should fail hard rather than discover a broken runtime at first spawn.
func (m *Manager) PreflightRuntime(ctx context.Context) error {
	return m.pod.Preflight(ctx)
}
```

- [ ] **Step 6: Rewrite `Manager.Stop` to use the backend**

In `Stop`, replace the two `m.rt.StopContainer(...)` calls with a single backend `Stop` (reconstruct a handle from the stored spawn). The floor-removal + mount-finalize + store-delete stay as-is:

```go
func (m *Manager) Stop(ctx context.Context, id string) error {
	sp, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	_ = m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: sp.SidecarID, AgentID: sp.AgentID})
	if sp.FloorIP != "" {
		if err := m.fw.Remove(ctx, firewall.Rules(sp.FloorIP, m.cfg.EgressAllowCIDRs)); err != nil {
			log.Printf("egress floor cleanup for %s (ip %s): %v", id, sp.FloorIP, err)
		}
	}
	for _, d := range sp.MountDirs {
		_ = m.backend.Finalize(ctx, d)
	}
	m.store.Delete(id)
	return nil
}
```

- [ ] **Step 7: Build + vet**

Run: `go build ./... && go vet ./internal/spawnlet/`
Expected: exit 0. There must be NO remaining `m.rt` reference. If the compiler reports `runtime` imported but unused, that would mean a missed reference — `runtime` IS still used (`runtime.PodSpec`/`AgentSpec`/`Resources`/`PodHandle`/`NewDockerPodBackend`), so it should stay imported. If `log` is reported unused, the `Stop` rewrite dropped its only use — re-check Step 6 kept the `log.Printf`.

- [ ] **Step 8: Run the full spawnlet regression suite (the key gate)**

Run: `go test ./internal/spawnlet/ -race -count=1 -v`
Expected: ALL PASS, unmodified — specifically:
- `TestCreateAppliesResourceLimits` / `TestNewManagerLimitDefaults` — `rt.Started[0]` (sidecar) + `rt.Started[1]` (agent) carry the mapped resources + runtime.
- `TestPreflightRuntime` — empty→nil, healthy runsc→nil, `rtErrOnRuntime`→error.
- `TestCreateFailClosedWhenFirewallFails` — `rt.Stopped["fake-1"]` true and `len(rt.Started)==1` (agent never starts; `pod.Stop` stops only the sidecar since the agent id is empty).
- `TestCreateSkipsFirewallSelfHostedDisabled` / `TestCreateCloudForcesEnforce` — floor honored per node class.
- `TestStopRemovesFloor` — `fw.Remove` called on Stop.
- `TestCreateRecordsNetnsPath` — `sp.NetnsPath == "/proc/4242/ns/net"`.
- `TestWSRelayEchoesViaFake` / `TestServerCreateSpawn` — unaffected.

If any fail, read the failure and the test; the rewrite must preserve the asserted behavior exactly — do NOT modify the test to fit. Fix the `manager.go` rewrite.

- [ ] **Step 9: Full module sweep**

Run: `go test ./... -race`
Expected: PASS across the module. `cmd/spawnlet` still builds against the unchanged `NewManager`/`PreflightRuntime` API.

- [ ] **Step 10: Commit**

```bash
git add internal/spawnlet/manager.go
git commit --no-verify -m "refactor(spawnlet): drive the pod lifecycle through runtime.PodBackend (sp-0wd)"
```

---

## Self-Review

**1. Spec coverage (spec §3.1/§3.2 + slice 2):**
- Two-phase `PodBackend` (`Ping`/`Preflight`/`StartPod`/`StartAgent`/`Stop`) — Task 1 `pod.go`. ✓
- `PodSpec`/`AgentSpec`/`Resources`/`PodHandle` (PodIP for floor, NetnsPath for attach, opaque backend ids) — Task 1. ✓
- Docker backend = sidecar owns netns, agent joins via `NetnsOf` — Task 1 `docker_pod.go`. ✓
- Manager keeps mounts/floor/store, drives `StartPod` → floor → `StartAgent`; no-unfirewalled-agent-window preserved (floor between the two phases) — Task 2. ✓
- `Stop` via the backend; `PreflightRuntime` delegates — Task 2. ✓
- Pure refactor, existing tests green unmodified (NewManager signature kept, wraps `rt`) — Task 2 Steps 1/8/9. ✓

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code; every run step has an exact command + expected result.

**3. Type consistency:** `PodBackend`/`PodSpec`/`AgentSpec`/`Resources`/`PodHandle` defined in Task 1 are used identically in Task 2. `NewDockerPodBackend(rt, runtimeName, smokeImage)` — same 3-arg shape in Task 1 (tests), Task 2 (`NewManager`). `PodHandle.{PodIP,NetnsPath,SidecarID,AgentID}` set by the backend (Task 1) and read by the Manager (Task 2). `StartAgent` mutates `h.AgentID` (Task 1) which the Manager then stores (Task 2). The egress-floor fail-closed sequence (`StartPod` → `fw.Apply` fail → `pod.Stop(h)` with empty `AgentID`) matches `TestDockerPodBackendStopSkipsEmptyAgentID` (Task 1) and `TestCreateFailClosedWhenFirewallFails` (Task 2 Step 8). ✓

**4. Non-goals (carried forward, not this slice):** the vestigial `AttachStdio: true` on the agent spec is preserved to keep the refactor pure (it is dead since slice 1's UDS transport — a future cleanup); the CRI backend, backend-selection-by-`CONTAINER_RUNTIME`, and the CRI floor are slices 3–5.
