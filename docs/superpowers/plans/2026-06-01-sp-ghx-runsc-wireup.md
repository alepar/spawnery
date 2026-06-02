# runsc Wire-up + End-to-End (sp-ghx) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Select the pod backend + egress floor by `CONTAINER_RUNTIME` (`DockerPodBackend`+`HostFloorApplier` for runc, `CRIPodBackend`+`CNIFloorApplier` for runsc), and document the host procedure to run the real goose+sidecar spawn under runsc — the round-trip `sp-vaw` blocked on.

**Architecture:** A new `Manager` constructor `NewManagerWithBackend(pod, fw, cfg)` injects an arbitrary `runtime.PodBackend` + `firewall.Applier` (the existing `NewManager(rt, cfg)` delegates to it with the Docker backend + `DOCKER-USER` floor). `cmd/spawnlet` gains a `buildManager(cfg)` that picks the runsc stack (CRI client + `SPAWNLET-EGRESS` floor) when `CONTAINER_RUNTIME=runsc`, else the Docker stack. The actual end-to-end runsc spawn requires a privileged containerd/runsc/CNI host, so it ships as a `MANUAL_VERIFICATION.md` procedure (the empirical `sp-vaw` closure).

**Tech Stack:** Go 1.26, the `internal/runtime/cri` + `internal/spawnlet/firewall` packages from slices 3–4, `cmd/spawnlet`.

**Scope:** Node only — `manager.go` (one new constructor), `cmd/spawnlet/main.go` (selection), and docs. No CP/proto/manifest/web changes. This dev sandbox has **no containerd/runsc/CNI/root** — the wire-up is hermetically testable here (CRI dial + Docker client are lazy, so construction succeeds without daemons); the real runsc spawn is host-only (documented, not run here). Commits use `--no-verify` (the `.beads` export hook dirties commits).

**`sp-vaw` note:** this slice completes the *code* path. `sp-vaw` (the empirical "agent reaches sidecar under runsc") closes only after the `MANUAL_VERIFICATION.md` steps are run on a real host — call that out, don't claim it's verified.

---

## File Structure

**Modified files:**
- `internal/spawnlet/manager.go` — add `NewManagerWithBackend(pod runtime.PodBackend, fw firewall.Applier, cfg)`; `NewManager(rt, cfg)` delegates to it.
- `cmd/spawnlet/main.go` — `buildManager(cfg)` selects the backend+floor by `CONTAINER_RUNTIME`; `main` uses it; new `CRI_ENDPOINT` / `CRI_RUNTIME_HANDLER` env.
- `deployment.md` — document `CRI_ENDPOINT` / `CRI_RUNTIME_HANDLER` in the node env table.
- `MANUAL_VERIFICATION.md` — a 🔒 host procedure for the runsc end-to-end + floor.

**New files:**
- `internal/spawnlet/manager_backend_test.go` — hermetic test of `NewManagerWithBackend` (injects a fake backend + fake applier).
- `cmd/spawnlet/main_test.go` — hermetic test that `buildManager` constructs both paths without error.

---

## Task 1: NewManagerWithBackend constructor

Let `cmd/spawnlet` inject any `PodBackend` + `Applier`. Refactor `NewManager` to delegate so the existing suite is unaffected.

**Files:**
- Modify: `internal/spawnlet/manager.go`
- Test: `internal/spawnlet/manager_backend_test.go`

- [ ] **Step 1: Write the failing test**

This reuses `fakePodBackend` (from `manager_sandbox_test.go`), `fakeApplier` (from `manager_egress_test.go`), and `writeApp` (from `manager_test.go`) — all in package `spawnlet`. Create `internal/spawnlet/manager_backend_test.go`:

```go
package spawnlet

import (
	"context"
	"testing"
)

func TestNewManagerWithBackendInjectsBackendAndFloor(t *testing.T) {
	fb := &fakePodBackend{}
	fa := &fakeApplier{}
	m := NewManagerWithBackend(fb, fa, ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true,
	})

	sp, err := m.Create(context.Background(), "spz", writeApp(t), "model")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The injected backend produced the handle (SandboxID from fakePodBackend.StartPod).
	if sp.SandboxID != "sandbox-x" {
		t.Fatalf("SandboxID = %q, want sandbox-x (injected backend not used)", sp.SandboxID)
	}
	// The injected floor applier was used (EgressEnforce=true -> Apply called).
	if !fa.applied {
		t.Fatal("injected firewall applier was not used on Create")
	}

	if err := m.Stop(context.Background(), sp.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fb.stopped == nil {
		t.Fatal("injected backend Stop not called")
	}
	if !fa.removed {
		t.Fatal("injected floor applier Remove not called on Stop")
	}
}

func TestNewManagerWithBackendAppliesDefaults(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir()})
	if m.cfg.SidecarPort != 8080 || m.cfg.MemLimitMB != 1024 || m.cfg.CPULimit != 1.0 || m.cfg.PidsLimit != 256 {
		t.Fatalf("defaults not applied: %+v", m.cfg)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/spawnlet/ -run TestNewManagerWithBackend -v`
Expected: FAIL — `NewManagerWithBackend` undefined.

- [ ] **Step 3: Refactor the constructor**

In `internal/spawnlet/manager.go`, replace the whole `NewManager` function with these two functions:

```go
// NewManager builds a Manager on the Docker/runc path: the Docker pod backend + the DOCKER-USER
// egress floor. (cmd/spawnlet uses NewManagerWithBackend for the runsc/CRI path.)
func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	return NewManagerWithBackend(
		runtime.NewDockerPodBackend(rt, cfg.ContainerRuntime, cfg.AgentImage),
		firewall.HostFloorApplier{},
		cfg,
	)
}

// NewManagerWithBackend builds a Manager around an explicit pod backend + egress-floor applier,
// applying config defaults. Used to select the runc (Docker + DOCKER-USER) vs runsc (CRI +
// SPAWNLET-EGRESS) stacks by CONTAINER_RUNTIME.
func NewManagerWithBackend(pod runtime.PodBackend, fw firewall.Applier, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	if cfg.MemLimitMB == 0 {
		cfg.MemLimitMB = 1024
	}
	if cfg.CPULimit == 0 {
		cfg.CPULimit = 1.0
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 256
	}
	return &Manager{
		pod:     pod,
		cfg:     cfg,
		store:   NewStore(),
		backend: storage.NewScratch(cfg.DataRoot),
		fw:      fw,
	}
}
```

- [ ] **Step 4: Run the test + the full spawnlet suite**

Run: `go test ./internal/spawnlet/ -race -count=1`
Expected: PASS — the 2 new tests pass and the existing suite is green unmodified (`NewManager` behavior unchanged — same Docker backend + `HostFloorApplier`, same defaults).

- [ ] **Step 5: Build + commit**

```bash
go build ./... && go vet ./internal/spawnlet/
git add internal/spawnlet/manager.go internal/spawnlet/manager_backend_test.go
git commit --no-verify -m "feat(spawnlet): NewManagerWithBackend injects pod backend + floor (sp-ghx)"
```

---

## Task 2: Select backend + floor by CONTAINER_RUNTIME in cmd/spawnlet

**Files:**
- Modify: `cmd/spawnlet/main.go`
- Test: `cmd/spawnlet/main_test.go`

- [ ] **Step 1: Add the imports + the selector**

In `cmd/spawnlet/main.go`, add `"fmt"` to the stdlib imports and these to the spawnery import group: `"spawnery/internal/runtime/cri"` and `"spawnery/internal/spawnlet/firewall"`. Then add this function (place it after `main`, near the other helpers):

```go
// buildManager selects the pod backend + egress floor by CONTAINER_RUNTIME: runsc -> a containerd
// CRI pod backend + the SPAWNLET-EGRESS floor; anything else -> the Docker backend + DOCKER-USER.
func buildManager(cfg spawnlet.ManagerConfig) (*spawnlet.Manager, error) {
	if cfg.ContainerRuntime == "runsc" {
		endpoint := env("CRI_ENDPOINT", "unix:///run/containerd/containerd.sock")
		client, err := cri.Dial(endpoint)
		if err != nil {
			return nil, fmt.Errorf("cri dial %s: %w", endpoint, err)
		}
		backend := cri.NewCRIPodBackend(client, env("CRI_RUNTIME_HANDLER", "runsc"))
		return spawnlet.NewManagerWithBackend(backend, firewall.NewCNIFloorApplier(), cfg), nil
	}
	rt, err := runtime.NewDocker()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	return spawnlet.NewManager(rt, cfg), nil
}
```

- [ ] **Step 2: Use the selector in main**

In `main`, replace the opening block (the `rt, err := runtime.NewDocker()` + `mgr := spawnlet.NewManager(rt, spawnlet.ManagerConfig{...})` construction) with: build the config into a variable, then call `buildManager`. The result:

```go
func main() {
	cfg := spawnlet.ManagerConfig{
		AgentImage:    env("AGENT_IMAGE", "spawnery/stubagent:dev"),
		SidecarImage:  env("SIDECAR_IMAGE", "spawnery/sidecar:dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		DataRoot:      env("DATA_ROOT", "/var/lib/spawnlet/spawns"),

		NodeClass:        env("NODE_CLASS", "cloud"),
		EgressEnforce:    getenvBool("EGRESS_ENFORCE", true),
		EgressAllowCIDRs: splitCSV(os.Getenv("EGRESS_ALLOW_CIDRS")),

		MemLimitMB:       getenvInt64("MEM_LIMIT_MB", 1024),
		CPULimit:         getenvFloat("CPU_LIMIT", 1.0),
		PidsLimit:        getenvInt64("PIDS_LIMIT", 256),
		ContainerRuntime: os.Getenv("CONTAINER_RUNTIME"),
		HardenRootfs:     getenvBool("HARDEN_ROOTFS", false),
	}
	mgr, err := buildManager(cfg)
	if err != nil {
		log.Fatalf("manager init: %v", err)
	}
	ctx := context.Background()
	if err := mgr.PreflightRuntime(ctx); err != nil {
		log.Fatalf("container runtime preflight failed: %v", err)
	}
	// ... (the rest of main — the CP_ADDR branch + standalone server — UNCHANGED)
```

Leave everything from `if cpURL := os.Getenv("CP_ADDR"); cpURL != "" {` onward exactly as it is.

- [ ] **Step 3: Write the hermetic selector test**

`cri.Dial` (grpc lazy connect) and `runtime.NewDocker` (lazy Docker client) both construct without a live daemon, so both paths are testable here. Create `cmd/spawnlet/main_test.go`:

```go
package main

import (
	"testing"

	"spawnery/internal/spawnlet"
)

func TestBuildManagerRunscPath(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		ContainerRuntime: "runsc", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runsc buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestBuildManagerDockerDefault(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("docker buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}
```

- [ ] **Step 4: Run + build + vet**

Run: `go test ./cmd/spawnlet/ -race -v`
Expected: PASS (both paths construct without error).
Run: `go build ./... && go vet ./cmd/spawnlet/`
Expected: exit 0.
Run: `go test ./... -race`
Expected: all PASS (whole module).

- [ ] **Step 5: Commit**

```bash
git add cmd/spawnlet/main.go cmd/spawnlet/main_test.go
git commit --no-verify -m "feat(spawnlet): select CRI+SPAWNLET-EGRESS vs Docker+DOCKER-USER by CONTAINER_RUNTIME (sp-ghx)"
```

---

## Task 3: Document the env + the runsc end-to-end host procedure

**Files:**
- Modify: `deployment.md`
- Modify: `MANUAL_VERIFICATION.md`

- [ ] **Step 1: Document the new env in deployment.md**

In `deployment.md` §4 (the Node `spawnlet` configuration env table), add two rows after the `CONTAINER_RUNTIME` row:

```markdown
| `CRI_ENDPOINT` | `unix:///run/containerd/containerd.sock` | only used when `CONTAINER_RUNTIME=runsc`: the containerd CRI socket the node dials. |
| `CRI_RUNTIME_HANDLER` | `runsc` | only used when `CONTAINER_RUNTIME=runsc`: the containerd runtime handler name registered in `config.toml`. |
```

- [ ] **Step 2: Add the runsc end-to-end procedure to MANUAL_VERIFICATION.md**

First READ `MANUAL_VERIFICATION.md` to match its section style (each section: a `## X. Title (beads)` heading, a **Summary.** paragraph, and a checklist; the legend uses 🔒 for "needs a privileged host (Docker+iptables+root)"). Add a new section at the END of the document:

```markdown
## Z. runsc one-sandbox pod end-to-end (`sp-ghx`, closes `sp-vaw`) 🔒

**Summary.** With `CONTAINER_RUNTIME=runsc`, the node runs the spawn pod as a single containerd CRI
sandbox (handler `runsc`) holding the sidecar + agent containers — so the agent reaches the sidecar
on `127.0.0.1:8080` (which a per-container gVisor pod cannot do, the `sp-vaw` blocker) — with the
egress floor on the `SPAWNLET-EGRESS` chain. Everything below the wire-up is hermetically tested; this
checklist is the **real-host** validation that closes `sp-vaw`. Needs a privileged host with
containerd + runsc + CNI (see `deployment.md` §5 for the containerd `config.toml` handler + CNI
conflist prerequisites).

**Host prep (one-time):**
```bash
# 1. runsc + shim on PATH; runsc CRI handler registered in /etc/containerd/config.toml:
#      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
#        runtime_type = "io.containerd.runsc.v1"
#    then: sudo systemctl restart containerd
# 2. CNI reference plugins in /opt/cni/bin + a bridge/firewall/portmap conflist in /etc/cni/net.d
# 3. images into containerd's k8s.io namespace (separate from Docker's moby):
make images   # builds spawnery/sidecar:dev + spawnery/goose:dev (Docker)
for img in spawnery/sidecar:dev spawnery/goose:dev; do \
  docker save "$img" | sudo ctr -n k8s.io images import - ; done
```

**Run the runsc spawn (standalone node + spawnctl):**
```bash
# Build the binaries, then run the node under runsc as root (needs CRI sock + iptables + setns):
make bin/spawnlet bin/spawnctl
sudo env "PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin" \
  CONTAINER_RUNTIME=runsc AGENT_IMAGE=spawnery/goose:dev SIDECAR_IMAGE=spawnery/sidecar:dev \
  OPENROUTER_API_KEY="$OPENROUTER_API_KEY" DATA_ROOT=/tmp/spawns \
  bin/spawnlet &                                  # standalone mode (no CP_ADDR)
printf 'What is the secret word?\n' | \
  bin/spawnctl -addr http://127.0.0.1:9090 -app examples/secret-app -model free
```

**Verify:**
- [ ] 🔒 The node logs a successful **runsc preflight** (CRI runtime + network ready) at startup; it
      exits hard if containerd/runsc/CNI is misconfigured (not at first spawn).
- [ ] 🔒 The spawn reaches **ACTIVE** and `spawnctl` gets a real model reply (e.g. "The secret word is
      …") — i.e. the agent reached the sidecar on `127.0.0.1:8080` **under runsc** (the `sp-vaw` fix).
- [ ] 🔒 `sudo crictl pods` / `crictl ps` show one pod sandbox (handler `runsc`) with two containers
      (sidecar + agent); `sudo iptables -S SPAWNLET-EGRESS` shows the per-pod `-s <podIP>` floor rules
      and `sudo iptables -S FORWARD | head -1` shows the `-j SPAWNLET-EGRESS` jump at position 1.
- [ ] 🔒 Inside the agent container, `curl --max-time 3 http://169.254.169.254/` and an RFC1918 host
      are **blocked** while public egress works — the floor enforces under the CRI pod (mirror of
      `just test-cni-egress`, but on the real runsc pod).
- [ ] 🔒 After `spawnctl`/stop, the pod sandbox is removed (`crictl pods` clean) and the per-pod
      `SPAWNLET-EGRESS` rules are gone (`iptables -S SPAWNLET-EGRESS` back to just the chain).

Once these pass on a host, **close `sp-vaw`** (the empirical gVisor-pod fix is confirmed).
```

- [ ] **Step 3: Commit**

```bash
git add deployment.md MANUAL_VERIFICATION.md
git commit --no-verify -m "docs: CRI_ENDPOINT env + runsc end-to-end host procedure (sp-ghx)"
```

---

## Self-Review

**1. Spec coverage (spec §5 slice 5):**
- Select backend + floor by `CONTAINER_RUNTIME` (Docker+DOCKER-USER vs CRI+SPAWNLET-EGRESS) → Task 1 (`NewManagerWithBackend`) + Task 2 (`buildManager`). ✓
- CRI preflight wired (the existing `mgr.PreflightRuntime` → `CRIPodBackend.Preflight` readiness check, called at startup, `log.Fatal` on failure) → unchanged `main`, now backed by the CRI backend. ✓
- The full goose+sidecar spawn under runsc + host-verify the floor + close `sp-vaw` → Task 3 (host procedure; can't run in this sandbox). ✓ — and the slice does NOT claim `sp-vaw` is verified (it closes after the host steps run).
- New env (`CRI_ENDPOINT`, `CRI_RUNTIME_HANDLER`) documented → Task 3. ✓

**2. Placeholder scan:** No TBD/TODO; every code step is complete; every run step has an exact command + expected result. The `MANUAL_VERIFICATION.md` host commands are concrete (not placeholders) — they're an executable procedure for the user.

**3. Type consistency:** `NewManagerWithBackend(pod runtime.PodBackend, fw firewall.Applier, cfg ManagerConfig)` (Task 1) is called by `buildManager` (Task 2) with `cri.NewCRIPodBackend(...)` + `firewall.NewCNIFloorApplier()` (both from slices 3–4) and by `NewManager` with `runtime.NewDockerPodBackend(...)` + `firewall.HostFloorApplier{}`. The hermetic tests reuse `fakePodBackend`/`fakeApplier`/`writeApp` (existing package-`spawnlet` helpers). `cri.Dial`/`cri.NewCRIPodBackend` signatures match slice 3; `firewall.NewCNIFloorApplier` matches slice 4.

**4. Deliberately host-only (not this sandbox):** the real runsc spawn, the actual `SPAWNLET-EGRESS` enforcement under a CRI pod, the netns-from-pid shape, and cgroup enforcement are validated by the Task-3 procedure on a privileged host — this is the final convergence of all the hermetic slices with real containerd. The wire-up (construction + selection) is fully testable here because `cri.Dial` and `runtime.NewDocker` are lazy.
