# Design: per-backend ACP transport for rootless self-hosted

> **Status:** design spec, 2026-06-01. Bead `sp-11p` (subsumes `sp-mcb`). **Reverses** the slice-1
> (`sp-7wa`) decision to unify both backends on the UDS `@spawnlet-acp` + `setns` transport. Companion
> to [`2026-06-01-runsc-cri-pod-backend-design.md`](2026-06-01-runsc-cri-pod-backend-design.md).

## 1. Goal & problem

**Goal:** **rootless self-hosted nodes on Mac *and* Linux** (including rootless **Podman** via its
Docker-compatible socket). Needing **root for cloud nodes is fine** (they enforce the egress floor).

**Problem:** slice 1 unified *both* backends on an in-pod UDS (`@spawnlet-acp`) reached via `setns`
into the pod netns. That transport:
- **needs `CAP_SYS_ADMIN` (root)** for `setns` — so self-hosted-without-root is impossible; and
- **cannot cross the Docker Desktop macOS↔Linux-VM boundary** — `setns` doesn't exist on macOS and the
  pod netns is in the VM; even a host-path bind-mounted Unix socket isn't connectable across virtiofs.

So both unprivileged self-hosted and **any** Mac Docker Desktop node are broken today. (TCP would
cross every boundary but exposes the ACP *control* channel on a shared bridge — a session-hijack
vector in multi-tenant — needing an ingress firewall + per-spawn port publishing; rejected.)

## 2. Decision: per-backend transport

The two deployment lanes don't need a shared transport — un-unify slice 1:

| Lane | Transport | Where | Root? | Exposure |
|---|---|---|---|---|
| **Docker/runc** (self-hosted: Mac, Linux, rootless Docker, rootless Podman) | **Docker stdio attach** (`rt.Attach` over the Docker API) | Mac + Linux | **No** | none (authenticated Docker API, no port) |
| **CRI/runsc** (cloud) | **UDS `@spawnlet-acp` + `setns`** (unchanged) | Linux only | Yes — but the floor needs root anyway | none (netns-scoped) |

Docker stdio attach is the **pre-epic** transport, restored for the Docker lane only: it rides the
Docker API that Docker Desktop forwards to the host, works against Podman's Docker-compat socket, and
needs no `setns`/port/netns.

**Deployment matrix after this change:**
- self-hosted **Mac / Linux / rootless Docker / rootless Podman** (`EGRESS_ENFORCE=false`) → **no root**
- **cloud** (CRI/runsc, Linux) → root, **only** for the egress floor

## 3. Mechanism

### 3.1 `PodBackend.Attach`
Add to the interface: `Attach(ctx context.Context, h *PodHandle) (*AttachedStream, error)`.
- **`DockerPodBackend.Attach`** → `d.rt.Attach(ctx, h.AgentID)` (Docker stdio attach; the agent runs
  with `AttachStdio:true`, already set in `StartAgent`).
- **`CRIPodBackend.Attach`** → `AttachACP(ctx, h.NetnsPath)` (the slice-1 UDS+setns, unchanged).

### 3.2 Routing the relay through the backend
- `Manager.Attach(ctx, sp *Spawn) (*runtime.AttachedStream, error)` reconstructs a `PodHandle`
  (`AgentID`/`NetnsPath`/`SidecarID`/`SandboxID`) and calls `m.pod.Attach`.
- `server.go` (`Session`), `ws.go` (`HandleWS`), and `node/attach.go` (`openSession`) call
  `s.m.Attach(ctx, sp)` / `a.mgr.Attach(ctx, sp)` instead of `runtime.AttachACP(ctx, sp.NetnsPath)`.
- The slice-1 injectable `Server.attach` field is **removed** — the white-box WS test now exercises
  the real `DockerPodBackend.Attach` path (the `FakeRuntime`'s `Attach` echoes stdin→stdout, so the
  relay echo test passes through the Docker backend without special injection).

### 3.3 Agent image entrypoint (conditional)
The same goose image serves both lanes via an `ACP_ADAPTER` env toggle in `deploy/agent/entrypoint.sh`:
```sh
# ... existing GOOSE_*/OPENAI_* env setup ...
if [ -n "$ACP_ADAPTER" ]; then
  exec /usr/local/bin/acpadapter goose acp   # CRI lane: PID 1 = adapter, in-pod UDS
else
  exec goose acp                             # Docker lane: PID 1 = goose, Docker-attachable
fi
```
- **`DockerPodBackend.StartAgent`** does *not* set `ACP_ADAPTER` → goose is PID 1 → Docker attach reaches it.
- **`CRIPodBackend.StartAgent`** appends `ACP_ADAPTER=1` to the agent container env → runs behind the adapter.
- **`deploy/stubagent/Dockerfile`** entrypoint reverts to `["/stubagent"]` (direct, Docker-attachable;
  the slice-1 adapter wrapper is dropped — stubagent is only used on the Docker lane).

### 3.4 Empty-IP tolerance (folds in `sp-mcb`)
Rootless Podman's default networking (slirp4netns / pasta) gives the container **no routable bridge
IP**, so `ContainerIP` is empty. (Loopback inside the shared netns still works, so agent↔sidecar on
`127.0.0.1:8080` is unaffected.)
- **`DockerPodBackend.StartPod`** fetches the IP **best-effort** — empty IP is not an error; the handle
  carries `PodIP:""`.
- **`Manager.Create`** fail-closes **only when enforcing without an IP**: `if egressEnforced() &&
  h.PodIP == "" { stop + finalize + error }`; a non-enforcing spawn proceeds with an empty IP.
- The **CRI backend keeps requiring** an IP (cloud pods always have a CNI IP, and they enforce).

## 4. Scope

**Changes:** `internal/runtime/pod.go` (interface), `docker_pod.go` (Attach + best-effort IP),
`cri/backend.go` (Attach + `ACP_ADAPTER=1`), `internal/spawnlet/manager.go` (`Attach` + Create
fail-closed-only-when-enforced-and-empty), `server.go`/`ws.go`/`node/attach.go` (route via
`Manager.Attach`; remove the `Server.attach` field), `deploy/agent/entrypoint.sh` (conditional),
`deploy/stubagent/Dockerfile` (direct), docs (`ISOLATION.md`/`deployment.md`: rootless matrix +
Podman; the slice-1 `CAP_SYS_ADMIN` claim becomes **CRI-lane-only**).

**Unchanged:** the CRI/runsc path's UDS+setns transport; the egress floor mechanisms; the
`acpadapter` binary (still used by the CRI lane).

**Caveats (host-verified, not in CI):** Podman's `ContainerAttach` over the compat API is the
finickiest endpoint — verify on a real rootless Podman host; rootless cgroup limits need cgroup-v2 +
systemd delegation.

## 5. Why not the alternatives
- **Host-path bind-mounted UDS:** doesn't cross Docker Desktop's virtiofs boundary — fails on Mac.
- **TCP everywhere:** crosses every boundary but exposes the ACP control channel on shared bridges
  (ingress-firewall + per-spawn port publishing); only wins if the node ever runs *remote* from the
  runtime, which it doesn't today.
- **Keep UDS+setns everywhere:** needs root, breaks Mac — the status quo we're fixing.
