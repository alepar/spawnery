# Design: runsc → containerd CRI pod backend

> **Status:** design spec, 2026-06-01. Epic `sp-7k8` (gVisor Phase-2 adoption), unblocks `sp-vaw`
> (runsc shared-netns blocker). Decision basis:
> [`2026-06-01-runsc-pod-backend-research-brief.md`](2026-06-01-runsc-pod-backend-research-brief.md)
> + the research results in `~/runsc-pods.md`. Companion to
> [`2026-06-01-gvisor-isolation-research-results.md`](2026-06-01-gvisor-isolation-research-results.md),
> [`ISOLATION.md`](../../../ISOLATION.md), [`deployment.md`](../../../deployment.md).

---

## 1. Problem

A spawn is a **pod** of two containers — an untrusted, user-steered coding **agent** (goose) and an
inference **sidecar** that holds the model API key — that **share one network namespace** (agent
reaches the sidecar on `127.0.0.1:8080`) but keep **separate mount/PID namespaces** (so the agent can
never read the sidecar's key). On the default `runc` runtime we build this with the Docker SDK: start
the sidecar, then start the agent with `--network=container:<sidecar>`.

We want the **same pod under gVisor (`runsc`)** for the cloud tier (kernel-attack-surface reduction;
see the gVisor research). **Per-container `--runtime=runsc` breaks the shared-netns trick** — each
runsc sandbox owns its own user-space netstack, so two runsc containers joined via
`--network=container:` get two independent loopbacks and cannot reach each other. Host-verified:
under runsc the agent boots and accepts ACP but cannot connect to the sidecar on `127.0.0.1:8080`
(`sp-vaw`).

The only working shape is the **Kubernetes pod model**: one Sentry hosting both containers with a
shared network namespace and separate mount/PID namespaces. This spec defines how the node
(`spawnlet`) creates, attaches to, firewalls, and tears down such a gVisor pod — **without a
Kubernetes cluster** — while keeping the `runc` path unchanged.

## 2. Decisions (and why)

All four are settled by the research (`~/runsc-pods.md`) and confirmed with the user.

| Decision | Choice | Why |
|---|---|---|
| **Integration layer** | **containerd CRI** via `k8s.io/cri-api` gRPC to the local containerd socket | Only path with primary-source evidence of multi-container shared-sandbox support under `containerd-shim-runsc-v1`. The containerd **native-client** path is ruled out (shim is hard-wired to CRI's "first container = pause" pattern; zero shipping references; runsc doesn't implement the containerd 2.x Sandbox API). Podman+runsc pods are broken (years-old open bugs). CRI-O is functionally equivalent but ties releases to k8s cadence; we standardize on containerd, which spawnlet already depends on. |
| **ACP stdio transport** | **Unix-domain socket in the pod netns** (not CRI `Attach`) | CRI's streaming server (SPDY/WebSockets) is built for `kubectl attach`; the WebSockets v5 path has had repeated hangs through 2025. A UDS gives clean EOF/half-close and avoids the streaming server entirely. |
| **Transport scope** | **Both backends** use the UDS (unified) | One ACP transport across runc and runsc; removes the Docker attach-demux path. The same agent image works under both. |
| **Egress floor** | **Keep `DOCKER-USER` for Docker**; add a **`SPAWNLET-EGRESS`** chain for the CRI path | `DOCKER-USER` is Docker-specific and not in-path for a CNI-bridge pod. We do not touch the host-verified Docker floor; the CRI floor is the structural twin on a spawnlet-owned `FORWARD` chain. |

**Selection knob:** `CONTAINER_RUNTIME` — empty → Docker backend (runc); `runsc` → CRI backend.

## 3. Architecture

### 3.1 The `PodBackend` seam

Both backends implement one interface. The `Manager` keeps everything shared (manifest parse, mount
prep, the egress floor, the ACP attach, the store); only the pod lifecycle is backend-specific.

```go
// internal/runtime/pod.go
type PodBackend interface {
    Ping(ctx context.Context) error
    Preflight(ctx context.Context) error
    // StartPod brings up the sandbox + the (trusted) sidecar. Returns the pod IP (for the floor)
    // and the netns path (for the ACP attach). The agent is NOT started yet.
    StartPod(ctx context.Context, s PodSpec) (*PodHandle, error)
    // StartAgent starts the (untrusted) agent container inside the existing pod.
    StartAgent(ctx context.Context, h *PodHandle, a AgentSpec) error
    Stop(ctx context.Context, h *PodHandle) error
}

type PodSpec struct {
    ID                       string
    SidecarImage             string
    SidecarEnv               []string
    Resources                Resources // MemoryBytes, NanoCPUs, PidsLimit
    Runtime                  string    // "" = Docker default; "runsc" for the CRI backend's handler
}

type AgentSpec struct {
    Image          string
    Env            []string
    Mounts         []Mount
    Resources      Resources
    DropAllCaps    bool
    ReadonlyRootfs bool
}

type PodHandle struct {
    ID        string // spawn id
    PodIP     string // for the egress floor (-s <podIP>)
    NetnsPath string // /proc/<pid>/ns/net, for AttachACP setns
    // backend-internal handles kept opaque to the Manager:
    sidecarID, agentID string   // Docker backend
    sandboxID          string   // CRI backend
}
```

**Two-phase create is load-bearing:** `StartPod` then (floor) then `StartAgent` guarantees the egress
floor is applied **before the untrusted agent runs** — no unfirewalled-agent window — for *both*
backends. The sidecar/pause that runs during the pre-floor window is trusted (ours).

`Manager.Create` becomes backend-agnostic:

```
prepare mounts (storage backend, unchanged)
h := backend.StartPod(ctx, podSpec)
if egressEnforced { floor.Apply(ctx, firewall.Rules(h.PodIP, allowCIDRs)) }  // fail-closed
backend.StartAgent(ctx, h, agentSpec)
store.Put(spawn{... handle ...})
```

`Manager.Stop`: `backend.Stop(h)` → `floor.Remove(...)` → finalize mounts (unchanged ordering).

### 3.2 Backends

**Docker backend** (`internal/runtime/docker_pod.go`) — wraps the existing `docker.go` primitives, no
new behavior:
- `StartPod`: start the sidecar; `PodIP` = `ContainerIP(sidecarID)`; `NetnsPath` =
  `/proc/<ContainerPID(sidecarID)>/ns/net`.
- `StartAgent`: start the agent with `NetnsOf=sidecarID` (joins the sidecar netns).
- `Stop`: stop agent, stop sidecar.

**CRI backend** (`internal/runtime/cri/`, its own subpackage to localize the `k8s.io/cri-api` dep):
- `StartPod`: `RunPodSandbox(PodSandboxConfig{runtime_handler:"runsc", ...})`; pull + `CreateContainer`
  + `StartContainer` for the sidecar; `PodIP` = `PodSandboxStatus(sandboxID).Network.IP`; `NetnsPath`
  = the sandbox's net ns (`PodSandboxStatus` → shim pid → `/proc/<pid>/ns/net`).
- `StartAgent`: pull + `CreateContainer` + `StartContainer` for the agent in `sandboxID`. The CRI
  plugin sets `io.kubernetes.cri.sandbox-id` / `container-type=container` automatically — exactly what
  runsc's subcontainer detection needs.
- `Stop`: `StopContainer(agent)`, `StopContainer(sidecar)`, `StopPodSandbox(sandboxID)` (runs CNI DEL,
  tears down the netns), `RemovePodSandbox(sandboxID)`.

### 3.3 ACP transport (UDS side-channel)

The agent's ACP no longer rides the container's main stdio.

- **In-container adapter** — a small entrypoint binary in the agent image (`deploy/agent/`). It
  listens on the **abstract** Unix socket `@spawnlet-acp`; on connect it execs `goose acp` and wires
  the socket ↔ goose's stdin/stdout (bidirectional copy). Abstract sockets are scoped to the
  **network namespace**, so the socket is reachable by anything sharing the pod netns and nothing
  outside it. goose itself is unchanged (still `goose acp` over stdio internally). One connection per
  spawn; on goose exit the adapter exits.
- **Node side** — `runtime.AttachACP(ctx, netnsPath) (*AttachedStream, error)`: open `netnsPath`,
  `setns(fd, CLONE_NEWNET)` on a locked OS thread, `net.Dial("unix", "@spawnlet-acp")`, restore the
  thread's netns, return an `AttachedStream{Stdin: conn, Stdout: conn, Close: conn.Close}`. Half-close
  = `conn.(*net.UnixConn).CloseWrite()`; teardown = socket EOF.
- **Consumers** (`internal/spawnlet/ws.go`, `server.go`) change from `rt.Attach(agentID)` to
  `runtime.AttachACP(handle.NetnsPath)` — the `AttachedStream` shape is preserved, so the ACP relay
  is otherwise untouched.

> **Threading note:** `setns(CLONE_NEWNET)` mutates the calling OS thread's netns. `AttachACP` must
> `runtime.LockOSThread()`, switch, dial, then switch back and unlock — or dial on a dedicated thread
> that is discarded. The dialed `net.Conn` keeps working after the thread restores its netns (the
> connection is already established).

### 3.4 Egress floor

Rule **content** is unchanged (`firewall.Rules(ip, allowCIDRs)` — ACCEPT DNS `:53` + allow-list,
DROP `169.254.0.0/16` + RFC1918, default-allow public). Only the chain + jump differ per backend.
`firewall.Applier` already abstracts Apply/Remove; we add a second implementation and the Manager
selects by backend.

- **Docker backend → `HostFloorApplier` (unchanged):** rules on `DOCKER-USER`, host-verified, leave
  `egress_e2e` exactly as is.
- **CRI backend → new `CNIFloorApplier`:** a spawnlet-owned chain `SPAWNLET-EGRESS` in the `filter`
  table.
  - **Boot (idempotent):** create `SPAWNLET-EGRESS`; `iptables -I FORWARD 1 -j SPAWNLET-EGRESS`
    (in front of CNI's own `-j CNI-FORWARD`); install the static DROP rules for metadata + RFC1918.
  - **Per spawn (keyed by pod IP):** insert `-s <podIP>` ACCEPT/RETURN rules for DNS + allow-list
    above the drops; tag with `-m comment --comment "spawnlet-pod-<id>"` for GC. Default-allow public
    by falling through to `CNI-FORWARD`.
  - **Teardown:** delete the `-s <podIP>` rules; the chain persists.
  - **Reconcile:** re-assert `FORWARD -I 1 -j SPAWNLET-EGRESS` on every daemon boot and on a periodic
    tick (CNI may re-create its chains on containerd restart — research Risk #3).
- **Enforces under runsc:** the host veth still sees the pod's frames; the "in-netns floor is a no-op
  under runsc" finding only applies to rules placed *inside* the sandbox netstack. On nftables hosts,
  `iptables-nft` translates the same rules; the floor is identical in effect.

### 3.5 Image store

The CRI plugin uses the containerd **`k8s.io`** namespace, separate from Docker's `moby`. The CRI
backend pulls via the CRI `ImageService.PullImage`. For locally-built dev images, bridge with
`docker save <img> | ctr -n k8s.io images import -`. This split is unavoidable while the runc path
stays on the Docker SDK; documented in `deployment.md`.

### 3.6 Preflight

`Manager.PreflightRuntime` (today: smoke `true` under the runtime) gains a CRI-backend path: assert
`runsc --version` + `containerd-shim-runsc-v1` on PATH + the containerd socket connectable + the
`runsc` handler registered (`RuntimeStatus` `RuntimeReady`/`NetworkReady`) + the CNI binaries present,
then run a **canary pod** (`RunPodSandbox` + a minimal container + `StopPodSandbox`) and assert it
completes under a few seconds. `cmd/spawnlet` still `log.Fatal`s on failure — a misconfigured runsc
host is caught at boot, not at first spawn.

## 4. Components / files

| Path | Responsibility |
|---|---|
| `internal/runtime/pod.go` | `PodBackend` interface, `PodSpec`/`AgentSpec`/`PodHandle`/`Resources`, `AttachACP` helper. |
| `internal/runtime/docker_pod.go` | Docker backend implementing `PodBackend` over existing `docker.go` primitives. |
| `internal/runtime/cri/` | CRI backend: `cri-api` gRPC client, pod lifecycle, image pull, CRI preflight. |
| `deploy/agent/acpadapter/` | In-container entrypoint adapter (`@spawnlet-acp` ↔ `goose acp` stdio). |
| `deploy/agent/Dockerfile` | Entrypoint switched to the adapter (goose invoked by it). |
| `internal/spawnlet/firewall/cni.go` | `CNIFloorApplier` (`SPAWNLET-EGRESS` chain) + boot/reconcile install. |
| `internal/spawnlet/manager.go` | Hold a `PodBackend` (not a bare `ContainerRuntime`); two-phase `Create`; floor + `AttachACP`; backend + floor selected by `CONTAINER_RUNTIME`. |
| `internal/spawnlet/ws.go`, `server.go` | `AttachACP(handle.NetnsPath)` in place of `rt.Attach(agentID)`. |
| `cmd/spawnlet/main.go` | Construct the backend + floor applier from `CONTAINER_RUNTIME`; CRI preflight. |
| `deployment.md`, `ISOLATION.md` | Host ops (containerd runsc handler, CNI conflist, image-store bridge) + isolation posture under the pod model. |

## 5. Epic decomposition (slices)

Ordered to **de-risk the UDS transport on the working runc path before any CRI code exists.** Each
slice produces working, testable software.

1. **Slice 1 — UDS ACP transport.** Agent entrypoint adapter (`@spawnlet-acp` ↔ `goose acp`) +
   `runtime.AttachACP` (setns/dial) + switch the **Docker** path to it (replace `rt.Attach`). Ships
   and is verifiable on runc alone. Keystone — proves the transport first.
2. **Slice 2 — `PodBackend` seam + Docker backend.** Extract the two-phase `PodBackend`; reimplement
   the Docker path behind it. Pure refactor; existing tests stay green. Manager holds a `PodBackend`.
3. **Slice 3 — CRI backend lifecycle.** `internal/runtime/cri` client: `RunPodSandbox` + 2×
   `CreateContainer`/`StartContainer` + `StopPodSandbox`/`RemovePodSandbox`; image pull into `k8s.io`;
   CRI preflight (canary pod). Gated behind `CONTAINER_RUNTIME=runsc`.
4. **Slice 4 — CRI egress floor + host ops.** `CNIFloorApplier`/`SPAWNLET-EGRESS` (boot + per-pod +
   reconcile); containerd runsc-handler + CNI conflist config; host docs.
5. **Slice 5 — wire-up + end-to-end.** Select backend + floor by env; run the full goose+sidecar spawn
   under runsc (the round-trip `sp-vaw` blocked on); host-verify the floor under the pod model.

## 6. Testing

- **Hermetic (CI):**
  - UDS adapter: socket ↔ stdio bidirectional-copy + EOF/half-close (a stub "goose" echo process).
  - `PodBackend` Docker impl: existing manager/runtime tests adapt; `FakeRuntime`/fake backend records
    the two-phase calls.
  - CRI client control flow: against a **fake CRI gRPC server** (the `cri-api` service interfaces),
    asserting the `RunPodSandbox`→`CreateContainer`×2→`Stop`/`Remove` sequence, image-pull-on-missing,
    and teardown ordering. `-race` clean.
  - `CNIFloorApplier` rule construction (chain/jump/`-s` match args) + fail-closed control flow,
    mirroring the existing `firewall` unit tests.
- **Build-tagged, host-gated (privileged node host, not CI sandbox):**
  - `AttachACP` real `setns` dial into a live netns.
  - CRI pod e2e: a real `runsc` pod, agent dials sidecar on `127.0.0.1:8080`, ACP round-trip,
    clean teardown (no leaked netns/images over N cycles).
  - `SPAWNLET-EGRESS` enforcement, extending `egress_e2e`: metadata + RFC1918 dropped, DNS + allow-list
    pass, public default-allows, and `iptables -F CNI-FORWARD` does not break the floor.

## 7. Risks & mitigations

| Risk | Mitigation |
|---|---|
| CRI streaming-attach instability | Avoided — ACP uses the UDS side-channel, not CRI `Attach`. |
| Image-store split-brain (`moby` vs `k8s.io`) | Pull runsc images through CRI; document the `docker save \| ctr -n k8s.io images import` bridge; monitor disk per host. |
| CNI re-creates its chains on containerd restart | Boot-time + periodic reconcile re-asserts `FORWARD -I 1 -j SPAWNLET-EGRESS`; an audit tick diffs expected vs actual. |
| containerd version / annotation / sandboxer drift | Pin the containerd minor version per fleet; gate upgrades behind the canary preflight. |
| runsc-shim multi-container subtleties (cgroup ordering vs `SystemdCgroup`) | Lock `SystemdCgroup` to the host's cgroup manager; ship shim debug logs. |
| `setns` thread-netns leak | `AttachACP` locks the OS thread, switches, dials, restores, unlocks (§3.3). |

## 8. Out of scope / gating

- **Gated alongside `sp-ova` (node auth):** runsc must not reach real multi-tenant cloud until the CP
  authenticates nodes and verifies cloud class (cloud-spoofing is the open P0). This epic builds the
  runtime; `sp-ova` guards who may run it.
- **Not in this epic:** containerd 2.x native-client backend (revisit if runsc gains Sandbox-API
  "sandboxer = shim" support); microVM (Kata/Firecracker) Phase 3; unifying the runc path off the
  Docker SDK; per-app egress allow-list. The Docker/runc floor (`DOCKER-USER`) is intentionally left
  untouched.
