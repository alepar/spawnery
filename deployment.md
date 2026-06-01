# Spawnery — Deployment & Environment Prerequisites

> **Status:** living doc, started 2026-06-01. Covers the **prod environment prerequisites** for
> running the control plane (CP) + nodes. The current ship target is the **Demo MVP** (Spawnery-
> operated home server, local/managed inference); items that are demo-only or not-yet-prod are
> called out. Keep this in sync as deployment surface changes.

## 1. Components

| Component | Binary / image | Runs where | Role |
|---|---|---|---|
| **Control plane (CP)** | `bin/cp` | one host (the SPOF today — see `sp-9um`) | client↔node relay, auth, scheduling, durable store (apps/spawns) |
| **Node (spawnlet)** | `bin/spawnlet` | each execution host | drives Docker pods, attaches the egress floor, relays ACP |
| **Sidecar** | `spawnery/sidecar:dev` image | per-spawn container | OpenAI-compatible inference proxy (holds the model key) |
| **Agent** | `spawnery/goose:dev` (or `stubagent`) image | per-spawn container | the app's agent; joins the sidecar's netns |
| **Web** | `web/` (vite) | static host / dev | client UI |

A **spawn** = a two-container pod (sidecar + agent) sharing one network namespace.

## 2. Host prerequisites

### 2.1 Build host (and CI)
- **Go** (toolchain per `go.mod`).
- **Docker** (to build the sidecar/agent images: `make images`).
- **Codegen tools** on `PATH` (only needed to regenerate protobuf, i.e. `make gen`): pinned
  `buf@v1.45.0`, `protoc-gen-go@v1.34.2`, `protoc-gen-connect-go@v1.16.2`. They live at
  `$(go env GOPATH)/bin` — `export PATH="$PATH:$(go env GOPATH)/bin"`.
- Build: `make build` (→ `bin/spawnlet`, `bin/spawnctl`; `make bin/cp` for the CP) and
  `make images` (→ `spawnery/sidecar:dev`, `spawnery/stubagent:dev`, `spawnery/goose:dev`).

### 2.2 Node (spawnlet) host
- **Docker daemon** reachable (the spawnlet uses the Docker SDK via `client.FromEnv` — it needs the
  Docker socket / `DOCKER_HOST`).
- **Cloud-class nodes additionally require the egress floor toolchain (§5):**
  - `iptables` and `nsenter` installed on the host,
  - the spawnlet process running with **`CAP_NET_ADMIN`** (in practice: root, or a capability grant),
    because it enters each pod's network namespace to install firewall rules.
  - Without these, a cloud node **fails closed** — spawns will not start (by design).
- The agent/sidecar **images** (`AGENT_IMAGE`, `SIDECAR_IMAGE`) must be present/pullable on the host.
- A writable `DATA_ROOT` for per-spawn mount dirs.

### 2.3 CP host
- A writable path or a Postgres instance for the store (§3.1).
- Network reachable by nodes and clients (the CP listens on `CP_LISTEN`).

## 3. Control plane (CP) configuration — env

| Var | Default | Notes |
|---|---|---|
| `CP_LISTEN` | `127.0.0.1:8080` | listen addr. **Prod: bind a routable addr behind TLS** (see §6). |
| `CP_STORE_DRIVER` | `sqlite` | `sqlite` (modernc, file/`:memory:`) or `postgres` (pgx). Postgres requires an explicit `CP_STORE_DSN`. |
| `CP_STORE_DSN` | `file:cp.db?_pragma=busy_timeout(5000)` | sqlite (modernc, pure-Go) or a `pgx` DSN for Postgres (the store supports both dialect trees). Migrations (goose) auto-apply on open. |
| `CP_DEV_TOKENS` | `dev-token=dev` | `token=owner` pairs, comma-separated. **Demo-only stub auth** — replaced by E4 OAuth (`sp-7h6`) in prod. |
| `CP_TELEMETRY` | `telemetry/events.jsonl` | content-free event log path; empty disables. |
| `CP_MAX_SPAWNS_PER_OWNER` | `5` | per-user concurrent-spawn cap; `CreateSpawn` → `ResourceExhausted` at/over it. `0` = unlimited. |

## 4. Node (spawnlet) configuration — env

| Var | Default | Notes |
|---|---|---|
| `CP_ADDR` | _(unset)_ | when set (e.g. `http://cp-host:8080`), the node runs **CP-attached** (dials the CP, no inbound listener). Unset → standalone mode (dev). |
| `NODE_ID` | `node-1` | node identity in the CP. |
| `NODE_CLASS` | `cloud` | **`cloud`** = Spawnery-operated → egress floor **always enforced, non-disableable**. **`self-hosted`** = the operator's box → floor honored but disableable via `EGRESS_ENFORCE`. Default `cloud` = an unconfigured node is restricted. |
| `EGRESS_ENFORCE` | `true` | only honored on `self-hosted` nodes (cloud always enforces). `false` on self-hosted runs spawns **unrestricted** (logged loudly). |
| `EGRESS_ALLOW_CIDRS` | _(empty)_ | comma-separated CIDRs `ACCEPT`ed before the block-floor drops — for operators whose model upstream / DNS resolver is on a LAN (RFC1918). |
| `AGENT_IMAGE` | `spawnery/stubagent:dev` | the agent container image. Prod: a pinned, real agent image (e.g. `spawnery/goose:<tag>`). |
| `SIDECAR_IMAGE` | `spawnery/sidecar:dev` | inference-proxy image. |
| `OPENROUTER_API_KEY` | _(unset)_ | passed to the sidecar; **secret — never commit** (see §7). |
| `DATA_ROOT` | `/var/lib/spawnlet/spawns` | per-spawn mount host dirs (Scratch backend today). |
| `SPAWNLET_ADDR` | `127.0.0.1:9090` | standalone-mode listen addr (ignored in CP-attached mode). |
| `MEM_LIMIT_MB` | `1024` | per-spawn memory cap (cgroup) on both pod containers. |
| `CPU_LIMIT` | `1.0` | per-spawn CPU cap (cores; cgroup NanoCPUs). |
| `PIDS_LIMIT` | `256` | per-spawn pids cap (cgroup, fork-bomb guard). |
| `CONTAINER_RUNTIME` | _(empty)_ | OCI runtime, e.g. `runsc` (gVisor) for stronger isolation. Empty = Docker default. **gVisor must be installed on the host** (not assumed); opt-in, not fail-closed. |

## 5. The egress floor (cloud nodes) — prereqs & verification

Cloud nodes enforce a per-pod network floor (bead `sp-rpa`): from the pod's netns, **drop**
cloud-metadata `169.254.0.0/16` + RFC1918 (`10/8`, `172.16/12`, `192.168/16`); allow loopback +
`EGRESS_ALLOW_CIDRS`; public egress otherwise (so the sidecar reaches `SIDECAR_UPSTREAM`). Applied
**after the sidecar starts, before the agent starts**, **fail-closed**.

**Host requirements:** `iptables`, `nsenter`, and `CAP_NET_ADMIN`/root for the spawnlet process.

**Verification (must run on a privileged host — NOT in the dev sandbox):**
```bash
go test -tags egress_e2e ./internal/spawnlet/firewall/ -run TestEgressFloorEnforced
```
This starts a real container, applies the floor, and asserts metadata + an RFC1918 host are
unreachable while a public host is reachable. The hermetic unit tests only cover rule-construction +
fail-closed control flow — **real packet-drop is unverified until this runs on the node host**
(tracked: "Verify egress floor enforcement on a privileged node host").

## 6. Sidecar configuration — env (injected by the spawnlet per spawn)

| Var | Default | Notes |
|---|---|---|
| `SIDECAR_UPSTREAM` | `https://openrouter.ai/api` | inference upstream. **Public** by default (so the egress floor needs no carve-out). If pointed at a **LAN model**, add its CIDR to `EGRESS_ALLOW_CIDRS`. |
| `OPENROUTER_API_KEY` | _(unset)_ | bearer key injected into upstream requests; the agent never sees it. |
| `SIDECAR_ADDR` | `127.0.0.1:8080` | sidecar listen addr (agent reaches it on loopback in the shared netns). |

## 7. Secrets

- **`OPENROUTER_API_KEY`** is the one live secret today. Keep it in a **gitignored `.env`** (the
  `Justfile` auto-loads it) or the node's secret store. **Never commit or print it.**
- In prod, model keys live only on the node/sidecar (CP relays opaque bytes; it never sees model
  traffic — caveat `sp-gtm` on CP-as-operator trust).

## 8. Bringing it up (reference)

Dev stack (single host) via `Justfile`:
```bash
just cp                 # control plane on 127.0.0.1:8080 (CP_DEV_TOKENS=dev-token=alice)
just node               # spawnlet attached to the CP (NODE_ID=node-1, goose agent)
just web                # web UI (vite)
# or: just dev          # all panes in mprocs
```
Prod (sketch — per-host): build + push images; run `bin/cp` with `CP_STORE_DSN`/`CP_LISTEN`/real
auth; on each node host run `bin/spawnlet` with `CP_ADDR`, `NODE_CLASS=cloud`, the egress toolchain
(§5), and `OPENROUTER_API_KEY` from a secret store.

## 9. Not-yet-prod / open items (be honest)

These are **demo gaps**, not prod-ready — track in beads:
- **Auth:** `CP_DEV_TOKENS` is a stub; real identity is E4 OAuth (`sp-7h6`).
- **TLS / channel security:** no per-session E2E channel or TLS termination story wired; CP key-
  vending MITM caveat (`sp-gtm`).
- **HA:** CP is a single point of failure / relay bandwidth cliff (`sp-9um`).
- **Storage:** Scratch backend only; managed GitHub/blob storage is E3 (`sp-u53`).
- **Isolation/quotas:** per-spawn cgroup limits (mem/cpu/pids) + per-user concurrency cap shipped
  (`sp-ach`); gVisor isolation is available via `CONTAINER_RUNTIME=runsc` but **opt-in and unverified
  in CI** (needs gVisor on the host). Real cgroup/gVisor enforcement, like the egress floor, must be
  validated on a privileged node host. The restricted-agent-toolset piece of `sp-eha` is still pending.
- **Node-class propagation:** the node knows its class; propagating it to the CP for routing/UX is
  in progress.
