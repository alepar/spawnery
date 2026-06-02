# Per-Backend ACP Transport (sp-11p) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give each pod backend its own ACP transport — Docker stdio attach for the Docker/runc lane (root-free, Mac + Linux + Podman), the existing UDS+setns for the CRI/runsc lane (cloud) — so self-hosted nodes run **without root**.

**Architecture:** Add `PodBackend.Attach(ctx, handle)`; the Docker impl restores `rt.Attach` (the pre-epic Docker API attach), the CRI impl keeps `AttachACP`. The relay routes through `Manager.Attach` → `m.pod.Attach`. The agent image entrypoint toggles on `ACP_ADAPTER` (Docker lane = `goose acp` as PID 1, attachable; CRI lane = behind the adapter on the in-pod UDS). The Docker backend tolerates an empty pod IP (rootless Podman), and `Manager.Create` fail-closes only when *enforcing* without an IP.

**Tech Stack:** Go 1.26; existing `internal/runtime`, `internal/runtime/cri`, `internal/spawnlet`, the agent images.

**Scope:** Node + agent images + docs. No CP/proto/web changes. Reverses slice 1's "unify on UDS" for the Docker lane. Subsumes `sp-mcb`. Spec: `docs/superpowers/specs/2026-06-01-per-backend-transport-design.md`. Commits use `--no-verify` (the `.beads` export hook dirties commits). This sandbox: hermetic Go tests run here; Docker-image builds + the real attach are host-gated. **Do not** run `git checkout -- .beads/` (reverts beads state); commit `.beads/issues.jsonl` if it blocks a commit.

---

## Task 1: PodBackend.Attach (interface + both impls + fakes)

Adding the method to the interface breaks every implementer until all are updated, so do them together to keep the build green.

**Files:**
- Modify: `internal/runtime/pod.go`, `internal/runtime/docker_pod.go`, `internal/runtime/cri/backend.go`
- Modify: `internal/spawnlet/manager_sandbox_test.go` (the `fakePodBackend`)
- Test: `internal/runtime/docker_pod_test.go`

- [ ] **Step 1: Write the failing test (Docker attach via the fake runtime)**

Append to `internal/runtime/docker_pod_test.go`:

```go
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
	// FakeRuntime.Attach echoes stdin->stdout.
	if _, err := att.Stdin.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := att.Stdout.Read(buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	_ = att.Close()
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/runtime/ -run TestDockerPodBackendAttach -v`
Expected: FAIL — `b.Attach` undefined.

- [ ] **Step 3: Add `Attach` to the interface**

In `internal/runtime/pod.go`, add to the `PodBackend` interface (after `Stop`):

```go
	// Attach returns the agent's ACP stdio stream. Docker backend = Docker stdio attach (no root);
	// CRI backend = the in-pod UDS via AttachACP (Linux + CAP_SYS_ADMIN).
	Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error)
```

- [ ] **Step 4: Implement Docker + CRI**

In `internal/runtime/docker_pod.go`, add:

```go
// Attach returns the agent's stdio via Docker's attach API — works on Mac (Docker Desktop) and Linux
// (incl. rootless Docker/Podman) without root, since it rides the Docker API, not setns.
func (d *DockerPodBackend) Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error) {
	return d.rt.Attach(ctx, h.AgentID)
}
```

In `internal/runtime/cri/backend.go`, add:

```go
// Attach returns the agent's ACP stdio via the in-pod UDS (setns into the pod netns). Linux + cloud.
func (b *CRIPodBackend) Attach(ctx context.Context, h *runtime.PodHandle) (*runtime.AttachedStream, error) {
	return runtime.AttachACP(ctx, h.NetnsPath)
}
```

(`internal/runtime/cri/backend.go` already imports `runtime`. The existing `var _ runtime.PodBackend = (*CRIPodBackend)(nil)` now also checks `Attach`.)

- [ ] **Step 5: Add `Attach` to the `fakePodBackend` test double**

In `internal/spawnlet/manager_sandbox_test.go`, add a method to `fakePodBackend` so it still satisfies `runtime.PodBackend`:

```go
func (f *fakePodBackend) Attach(_ context.Context, _ *runtime.PodHandle) (*runtime.AttachedStream, error) {
	pr, pw := io.Pipe()
	return &runtime.AttachedStream{Stdin: pw, Stdout: pr, Close: pw.Close}, nil
}
```

Add `"io"` to that test file's imports.

- [ ] **Step 6: Run + build**

Run: `go test ./internal/runtime/ -run TestDockerPodBackendAttach -race -v` → PASS.
Run: `go build ./... && go test ./... -race` → all green (the interface change compiles across all implementers, incl. `fakePodBackend`).

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/pod.go internal/runtime/docker_pod.go internal/runtime/cri/backend.go internal/spawnlet/manager_sandbox_test.go internal/runtime/docker_pod_test.go
git commit --no-verify -m "feat(runtime): PodBackend.Attach — Docker stdio attach vs CRI UDS (sp-11p)"
```

---

## Task 2: Route the relay through Manager.Attach

**Files:**
- Modify: `internal/spawnlet/manager.go`, `internal/spawnlet/server.go`, `internal/spawnlet/ws.go`, `internal/node/attach.go`, `internal/spawnlet/ws_test.go`

- [ ] **Step 1: Add `Manager.Attach`**

In `internal/spawnlet/manager.go`, add:

```go
// Attach returns the agent's ACP stdio for a spawn, dispatching to the backend's transport (Docker
// stdio attach for the Docker lane, the in-pod UDS for the CRI lane).
func (m *Manager) Attach(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
	return m.pod.Attach(ctx, &runtime.PodHandle{
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	})
}
```

- [ ] **Step 2: Remove the `Server.attach` field; route Session through `m.Attach`**

In `internal/spawnlet/server.go`:
- Delete the `attach func(...)` field from the `Server` struct.
- Simplify `NewServer` to `func NewServer(m *Manager) *Server { return &Server{m: m} }` (drop the closure default).
- In `Session`, change `att, err := s.attach(ctx, sp)` to `att, err := s.m.Attach(ctx, sp)`.

(Keep the `runtime` import only if still used — it is, for `*runtime.AttachedStream`? Session uses `att` inferred; check `go build`. If `runtime` becomes unused in server.go, remove it.)

- [ ] **Step 3: Route ws.go + node/attach.go**

- In `internal/spawnlet/ws.go`, change `att, err := s.attach(ctx, sp)` to `att, err := s.m.Attach(ctx, sp)`.
- In `internal/node/attach.go`, change line ~150 `att, err := runtime.AttachACP(ctx, sp.NetnsPath)` to `att, err := a.mgr.Attach(ctx, sp)`. Then check node/attach.go's imports: if `"spawnery/internal/runtime"` becomes unused, remove it (run `go build`/`go vet` to confirm).

- [ ] **Step 4: Update the white-box WS test**

In `internal/spawnlet/ws_test.go`, **remove** the injection block (the `srv.attach = func(ctx, sp) {...}` lines and its WHY comment) added in slice 1. The relay now echoes through the real path: `NewManager(f)` builds a `DockerPodBackend(f)`, so `m.Attach` → `DockerPodBackend.Attach` → `f.Attach(sp.AgentID)` → the FakeRuntime's echo. If `runtime` becomes an unused import in ws_test.go after removing the block, drop it.

- [ ] **Step 5: Build + test**

Run: `go build ./... && go vet ./internal/spawnlet/ ./internal/node/`
Expected: exit 0. Confirm no remaining `s.attach` / `runtime.AttachACP` reference in the relay paths: `grep -rn "s.attach\|AttachACP" internal/spawnlet/ internal/node/` should show AttachACP only inside `cri` (the backend) and `internal/runtime` (the impl), **not** in server.go/ws.go/node/attach.go.
Run: `go test ./internal/spawnlet/ -race -count=1 -v` → all pass, incl. `TestWSRelayEchoesViaFake` (now echoing through the Docker backend).
Run: `go test ./... -race` → all green.

- [ ] **Step 6: Commit**

```bash
git add internal/spawnlet/manager.go internal/spawnlet/server.go internal/spawnlet/ws.go internal/node/attach.go internal/spawnlet/ws_test.go
git commit --no-verify -m "feat(spawnlet): relay attaches via Manager.Attach -> backend (sp-11p)"
```

---

## Task 3: Tolerate an empty pod IP (rootless Podman) — closes sp-mcb

**Files:**
- Modify: `internal/runtime/docker_pod.go`, `internal/runtime/docker_pod_test.go`, `internal/spawnlet/manager.go`
- Test: `internal/spawnlet/manager_egress_test.go`

- [ ] **Step 1: Make the Docker backend's StartPod IP/PID best-effort**

In `internal/runtime/docker_pod.go` `StartPod`, replace the `ContainerPID` + `ContainerIP` blocks (the ones that currently `StopContainer` + return an error on failure) with best-effort lookups (rootless Podman gives no bridge IP, and the Docker lane needs neither for attach):

```go
	// Best-effort: rootless Podman (slirp4netns/pasta) has no bridge IP, and the Docker lane attaches
	// via the Docker API (not setns), so a missing IP/PID is not fatal here. The Manager fail-closes
	// later only if the egress floor is enforced and there's no IP to scope it.
	ip, _ := d.rt.ContainerIP(ctx, sidecarID)
	var netnsPath string
	if pid, perr := d.rt.ContainerPID(ctx, sidecarID); perr == nil {
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", pid)
	}
	return &PodHandle{
		PodIP:     ip,
		NetnsPath: netnsPath,
		SidecarID: sidecarID,
	}, nil
```

(The existing happy-path test `TestDockerPodBackendStartPodStartAgentStop` still passes — the `FakeRuntime` returns `172.17.0.99` + pid `4242`.)

- [ ] **Step 2: Update the cleanup test → tolerance test**

In `internal/runtime/docker_pod_test.go`, replace `TestDockerPodBackendStartPodCleansUpSidecarOnFailure` (and its `errOnPID`/`errOnIP` subtests, which asserted StartPod errors on PID/IP failure — no longer true) with a tolerance test. Keep the `errOnPID`/`errOnIP` helper types if they're defined there; the new test asserts StartPod **succeeds** with empty fields:

```go
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
```

If `errOnPID`/`errOnIP` are NOT already defined in this test file (they were added in a slice-3-era polish to a different file), define them here:

```go
type errOnPID struct{ *FakeRuntime }

func (r errOnPID) ContainerPID(ctx context.Context, id string) (int, error) { return 0, errors.New("no pid") }

type errOnIP struct{ *FakeRuntime }

func (r errOnIP) ContainerIP(ctx context.Context, id string) (string, error) { return "", errors.New("no ip") }
```

(Ensure `"errors"` is imported. If duplicate definitions occur because they already exist in this file, reuse the existing ones instead.)

- [ ] **Step 3: Run the runtime tests**

Run: `go test ./internal/runtime/ -race -count=1 -v` → PASS (the tolerance test + the unchanged happy path).

- [ ] **Step 4: Fail-closed only when enforcing without an IP (Manager.Create)**

In `internal/spawnlet/manager.go` `Create`, change the floor block so a non-enforcing spawn proceeds with an empty IP, but an *enforcing* spawn without an IP fails closed:

```go
	var floorIP string
	if m.egressEnforced() {
		if h.PodIP == "" {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): no pod IP to scope the floor")
		}
		if ferr := m.fw.Apply(ctx, firewall.Rules(h.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
		floorIP = h.PodIP
	}
```

- [ ] **Step 5: Test the Manager's empty-IP behavior**

Append to `internal/spawnlet/manager_egress_test.go` (it already has `fakeApplier`; add an empty-IP runtime). Add a small fake runtime whose `ContainerIP` is empty:

```go
type emptyIPRuntime struct{ *runtime.FakeRuntime }

func (emptyIPRuntime) ContainerIP(context.Context, string) (string, error) { return "", nil }

func TestCreateEmptyIPFailsClosedWhenEnforced(t *testing.T) {
	rt := emptyIPRuntime{runtime.NewFake()}
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), EgressEnforce: true})
	if _, err := m.Create(context.Background(), "sp1", "../../examples/secret-app", "model"); err == nil {
		t.Fatal("enforcing spawn with no pod IP must fail closed")
	}
}

func TestCreateEmptyIPSucceedsWhenNotEnforced(t *testing.T) {
	rt := emptyIPRuntime{runtime.NewFake()}
	m := NewManager(rt, ManagerConfig{AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(), NodeClass: "self-hosted", EgressEnforce: false})
	if _, err := m.Create(context.Background(), "sp2", "../../examples/secret-app", "model"); err != nil {
		t.Fatalf("non-enforcing spawn with no pod IP should succeed: %v", err)
	}
}
```

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/spawnlet/ ./internal/runtime/ -race -count=1` → all PASS. `go build ./...` → exit 0.

```bash
git add internal/runtime/docker_pod.go internal/runtime/docker_pod_test.go internal/spawnlet/manager.go internal/spawnlet/manager_egress_test.go
git commit --no-verify -m "feat: tolerate empty pod IP (rootless Podman); fail-closed only when enforcing (sp-11p, closes sp-mcb)"
```

---

## Task 4: Agent entrypoint toggle + CRI sets ACP_ADAPTER + stubagent direct

**Files:**
- Modify: `deploy/agent/entrypoint.sh`, `internal/runtime/cri/backend.go`, `internal/runtime/cri/backend_test.go`, `deploy/stubagent/Dockerfile`

- [ ] **Step 1: Make the goose entrypoint conditional**

In `deploy/agent/entrypoint.sh`, replace the final line `exec /usr/local/bin/acpadapter goose acp` with:

```sh
# Docker lane: goose is PID 1 (the node attaches via the Docker API). CRI lane sets ACP_ADAPTER=1 so
# goose runs behind the in-pod UDS adapter.
if [ -n "$ACP_ADAPTER" ]; then
  exec /usr/local/bin/acpadapter goose acp
else
  exec goose acp
fi
```

- [ ] **Step 2: CRI backend sets ACP_ADAPTER=1 on the agent**

In `internal/runtime/cri/backend.go` `StartAgent`, the agent `ContainerConfig.Envs` is built from `spec.Env` via `toKeyValues`. Add `ACP_ADAPTER=1` for the CRI lane. Change the `Envs:` line in the agent `CreateContainer` config from `Envs: toKeyValues(spec.Env),` to:

```go
		Envs: toKeyValues(append(spec.Env, "ACP_ADAPTER=1")),
```

(This is local to StartAgent — it appends to a copy passed to `toKeyValues`; it does not mutate the caller's slice in a way that matters since `spec` is passed by value. To be safe against aliasing, build it explicitly:)

```go
		Envs: toKeyValues(append([]string{"ACP_ADAPTER=1"}, spec.Env...)),
```

Use the explicit form above (prepend into a fresh slice — no aliasing).

- [ ] **Step 3: Assert it in the CRI lifecycle test**

In `internal/runtime/cri/backend_test.go`, in `TestStartAgentAndStopLifecycle`, after the agent config is captured (`ag := f.created[1]`), add an assertion that the agent env carries the adapter toggle:

```go
	var hasAdapter bool
	for _, kv := range ag.Envs {
		if kv.Key == "ACP_ADAPTER" && kv.Value == "1" {
			hasAdapter = true
		}
	}
	if !hasAdapter {
		t.Fatalf("CRI agent must set ACP_ADAPTER=1; envs=%+v", ag.Envs)
	}
```

- [ ] **Step 4: stubagent runs directly (Docker-attachable)**

In `deploy/stubagent/Dockerfile`, change the entrypoint so the stub agent is PID 1 (it's only used on the Docker lane, which attaches via the Docker API). Replace the build + entrypoint so it no longer wraps with the adapter:

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /stubagent ./cmd/stubagent
FROM gcr.io/distroless/static
COPY --from=build /stubagent /stubagent
ENTRYPOINT ["/stubagent"]
```

(READ the current `deploy/stubagent/Dockerfile` first; preserve its real base images / build flags, and only drop the `/acpadapter` build line + change the ENTRYPOINT to `["/stubagent"]`. If the golang builder tag differs, keep the real one.)

- [ ] **Step 5: Verify (Go build hermetic; image build host-gated)**

Run: `go build ./... && go test ./internal/runtime/cri/ -race -count=1 -v` → PASS (incl. the new ACP_ADAPTER assertion).
Run: `go vet ./internal/runtime/cri/` → exit 0.
**Host-gated (do NOT run here — no Docker):** on a host, `make .make/img-goose .make/img-stubagent` builds; `docker run --rm spawnery/goose:dev` runs goose directly (no ACP_ADAPTER), and with `-e ACP_ADAPTER=1` runs behind the adapter. Note in the report that you did NOT run Docker.

- [ ] **Step 6: Commit**

```bash
git add deploy/agent/entrypoint.sh internal/runtime/cri/backend.go internal/runtime/cri/backend_test.go deploy/stubagent/Dockerfile
git commit --no-verify -m "feat(images): goose entrypoint toggles ACP_ADAPTER; CRI sets it; stubagent direct (sp-11p)"
```

---

## Task 5: Docs — rootless self-hosted matrix + Podman

**Files:**
- Modify: `ISOLATION.md`, `deployment.md`

- [ ] **Step 1: Correct the CAP_SYS_ADMIN claim in ISOLATION.md**

In `ISOLATION.md` §3.4, find the slice-1 "**ACP transport (UDS)**" bullet that says the spawnlet needs `CAP_SYS_ADMIN`. Replace it with a per-backend version:

```markdown
- **ACP transport (per backend):** the **Docker/runc** lane attaches to the agent's stdio via the
  **Docker API** (`ContainerAttach`) — no `setns`, no root, and it works on macOS (Docker Desktop)
  and Linux (incl. rootless Docker/Podman). The **CRI/runsc** lane uses the in-pod **abstract UDS**
  (`@spawnlet-acp`) reached via `setns`, which needs **`CAP_SYS_ADMIN`** — but that lane is the
  Linux cloud node, which already runs as root for the egress floor. So `CAP_SYS_ADMIN` is required
  **only** on cloud (CRI) nodes; self-hosted Docker nodes run unprivileged.
```

- [ ] **Step 2: Add a rootless self-hosted section to deployment.md**

In `deployment.md`, add a subsection near §2.2 (node host) or §4 (node env). Add this (place it where node-host prerequisites or modes are described — READ the doc and pick the natural spot):

```markdown
### Rootless self-hosted nodes (no root)

A **self-hosted Docker/runc node runs without root** — the only thing that needed root was the egress
floor (iptables), which self-hosted disables. Config:

```bash
NODE_CLASS=self-hosted
EGRESS_ENFORCE=false      # the floor is the only root-needing piece; opt out (it can't run on macOS anyway)
# CONTAINER_RUNTIME unset → Docker/runc lane (the ACP relay uses the Docker API, no setns/root)
```

Docker access flavors (all unprivileged spawnlet process):
- **Docker group / Docker Desktop (macOS):** the spawnlet talks to the daemon over the Docker socket.
- **Rootless Docker:** point `DOCKER_HOST` at the rootless socket — no privileged daemon, no docker group.
- **Rootless Podman:** Podman exposes a Docker-compatible socket; point the node at it —
  `DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock` (run `podman system service` first). No
  new backend; Podman *is* the Docker backend over a different socket. Caveats: rootless networking
  (slirp4netns/pasta) gives the container **no bridge IP** (the node tolerates this — the floor is off
  anyway, and agent↔sidecar loopback is unaffected); rootless cgroup limits need cgroup-v2 + systemd
  delegation; `ContainerAttach` over Podman's compat API should be verified on the host.

The **CRI/runsc cloud lane still needs root** (Linux), but only for the egress floor — that's the
multi-tenant enforcing node where root is expected.
```

- [ ] **Step 3: Commit**

```bash
git add ISOLATION.md deployment.md
git commit --no-verify -m "docs: rootless self-hosted (Docker/rootless Docker/Podman) + per-backend transport (sp-11p)"
```

---

## Self-Review

**1. Spec coverage (spec §3):**
- `PodBackend.Attach` (Docker=`rt.Attach`, CRI=`AttachACP`) → Task 1. ✓
- Relay routes via `Manager.Attach`; `Server.attach` removed → Task 2. ✓
- Empty-IP tolerance + fail-closed-only-when-enforcing (closes `sp-mcb`) → Task 3. ✓
- Agent entrypoint `ACP_ADAPTER` toggle; CRI sets it; stubagent direct → Task 4. ✓
- Docs: `CAP_SYS_ADMIN` now CRI-lane-only; rootless self-hosted + Podman matrix → Task 5. ✓

**2. Placeholder scan:** No TBD/TODO; every code step is complete; every run step has an exact command + expected result. Host-gated image steps are explicitly marked do-not-run-here.

**3. Type consistency:** `PodBackend.Attach(ctx, *PodHandle) (*AttachedStream, error)` is defined in Task 1 and implemented by `DockerPodBackend`/`CRIPodBackend`/`fakePodBackend`; `Manager.Attach(ctx, *Spawn)` (Task 2) builds a `PodHandle` from the stored `Spawn` and calls it. The Docker `StartPod` best-effort IP/PID (Task 3) keeps returning a `*PodHandle`; `Manager.Create`'s enforcing-without-IP guard (Task 3) reads `h.PodIP`. The CRI `ACP_ADAPTER=1` env (Task 4) flows through `toKeyValues` into the agent `ContainerConfig.Envs`, matching the entrypoint's `$ACP_ADAPTER` check.

**4. Reversal note:** this un-does slice 1's "unify both backends on UDS" *for the Docker lane only* — the CRI lane's UDS+setns is unchanged, and `AttachACP` + the `acpadapter` binary stay (now CRI-only). The agent image serves both lanes via one conditional entrypoint.
