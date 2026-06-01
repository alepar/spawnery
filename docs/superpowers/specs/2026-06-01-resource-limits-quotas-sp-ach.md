# Resource Limits + Per-User Quota + Isolation Runtime (sp-ach / sp-eha core) — Design

**Bead:** `sp-ach` (resource limits/quotas) — also addresses the isolation-runtime hook of `sp-eha`
**Status:** Draft v1 — autonomous security-floor track (no blocking decision)
**Date:** 2026-06-01
**Builds on:** egress floor `sp-rpa` (`1435585`)

## 0. Context

The egress floor (`sp-rpa`) closed network abuse; this slice adds the **resource** half of the
security floor: per-spawn **cgroup limits** (mem/CPU/pids) so a hostile agent can't exhaust the host,
a **per-user concurrency cap** so one user can't spawn unboundedly, and an **optional gVisor runtime**
knob (the `sp-eha` isolation-hardening hook). Pure additive; node-side limits + a CP-side cap.

**Testability:** rule/config construction is hermetic; actual cgroup enforcement + gVisor need a
privileged host (same caveat as the egress floor — verify on the node host).

## 1. Scope

**In:**
1. **Per-spawn cgroup limits** — Docker `HostConfig.Resources` Memory / NanoCPUs / PidsLimit on
   **both** pod containers (sidecar + agent), from node config with sensible defaults.
2. **Isolation runtime knob** — `HostConfig.Runtime` (e.g. `runsc`/gVisor) when configured; default
   = Docker default (gVisor not assumed installed). The `sp-eha` isolation hook.
3. **Per-user concurrency cap** — CP rejects `CreateSpawn` with `ResourceExhausted` when the owner
   already has `>= cap` non-deleted spawns. `0` = unlimited.

**Out:** disk quotas (Scratch is ephemeral; revisit with E3) · cgroup v1-vs-v2 specifics (Docker
abstracts it) · GPU limits · restricted agent toolset (E1/agent image) · per-app (vs per-user)
quotas · dynamic/elastic limits.

## 2. Node-side: limits + runtime

### 2.1 `runtime.ContainerSpec` (+ Docker mapping)
Add fields:
```go
	MemoryBytes int64 // 0 = unlimited
	NanoCPUs    int64 // 0 = unlimited; 1 CPU = 1_000_000_000
	PidsLimit   int64 // 0 = unlimited
	Runtime     string // "" = Docker default; e.g. "runsc"
```
Extract a pure `buildHostConfig(s ContainerSpec) *container.HostConfig` (so the mapping is
unit-testable) that sets `NetworkMode`/`Binds` as today PLUS `Resources.Memory`,
`Resources.NanoCPUs`, `Resources.PidsLimit` (`*int64`), and `Runtime` — each only when non-zero/non-empty.

### 2.2 `ManagerConfig` + `manager.Create`
Add `MemLimitMB int64`, `CPULimit float64`, `PidsLimit int64`, `ContainerRuntime string`.
`NewManager` defaults: **MemLimitMB 1024, CPULimit 1.0, PidsLimit 256** (0/empty = unlimited if
explicitly set to 0). `Create` sets the computed limits + runtime on BOTH the sidecar and agent
`ContainerSpec` (NanoCPUs = `int64(CPULimit * 1e9)`).

### 2.3 `cmd/spawnlet` env
`MEM_LIMIT_MB`, `CPU_LIMIT`, `PIDS_LIMIT`, `CONTAINER_RUNTIME` → `ManagerConfig` (defaults as above;
`CONTAINER_RUNTIME` default empty).

## 3. CP-side: per-user concurrency cap

- `Server` gains an unexported `maxSpawnsPerOwner int` + exported `SetMaxSpawnsPerOwner(n int)`
  (avoids changing `NewServer`'s signature / its callers). Default `0` = unlimited.
- `CreateSpawn`: right after auth, if `maxSpawnsPerOwner > 0`, `count := len(ListByOwner(owner))`
  (non-deleted); if `count >= cap` → `connect.CodeResourceExhausted` ("spawn limit reached"). Reject
  **before** minting the spawn / provisioning.
- `cmd/cp`: `s.SetMaxSpawnsPerOwner(envInt("CP_MAX_SPAWNS_PER_OWNER", 5))`.

## 4. Node class

Limits + cap apply **always** (benign defaults); they are resource hygiene, not opt-in. The gVisor
runtime is **opt-in** (default off) since it requires a host with `runsc` installed — cloud
operators enable `CONTAINER_RUNTIME=runsc` where available. (No fail-closed coupling like egress —
an absent gVisor would otherwise break every spawn; document the operator responsibility.)

## 5. Testing

- **`buildHostConfig`** (hermetic unit): given a spec with limits+runtime, the `HostConfig` has
  `Resources.Memory`, `NanoCPUs`, `*PidsLimit`, `Runtime` set; zero values omitted; NetworkMode/Binds
  preserved.
- **Manager** (FakeRuntime): `Create` records two `ContainerSpec`s (sidecar+agent) both carrying the
  configured limits + runtime.
- **Per-user cap** (CP, hermetic): seed an owner with `cap` non-deleted spawns in the store, set
  `SetMaxSpawnsPerOwner(cap)`, `CreateSpawn` → `ResourceExhausted`; with cap `0`, no limit.
- **Verify-on-host (documented, not run here):** real mem/pids enforcement + gVisor isolation need a
  privileged Docker host (no hermetic packet/cgroup test in this sandbox).

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| Q.1 | Limit defaults | mem 1024 MB, 1.0 CPU, 256 pids; per-spawn, both containers; 0 = unlimited |
| Q.2 | Mapping testability | extract `buildHostConfig` (pure) — hermetic unit test |
| Q.3 | gVisor | `CONTAINER_RUNTIME` knob, default off (not assumed installed); opt-in, NOT fail-closed |
| Q.4 | Per-user cap | CP-side `CreateSpawn` count of non-deleted `ListByOwner` ≥ cap → `ResourceExhausted`; default 5; 0 = unlimited |
| Q.5 | Wiring | `Server.SetMaxSpawnsPerOwner` setter (no `NewServer` signature change) |
| Q.6 | Node class | limits/cap always on; gVisor opt-in (no fail-closed) |
