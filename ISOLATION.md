# Spawnery — Isolation Posture & Configurables

> **Status:** living doc, started 2026-06-01. Describes how a spawn (a user's agent running an app's
> code/persona on data) is isolated, what the floor enforces, what is deliberately *not* restricted,
> and every knob. Companion to [`deployment.md`](deployment.md) (ops) — this doc is the security view.

## 1. Threat model

A spawn runs a **coding-capable agent** (shell/file/network tools) that is **steered turn-by-turn by
an untrusted user**, executing an app's persona/skills, possibly authored by an untrusted third
party, on the user's data. So **the user-steered agent is itself the untrusted-code path** — we treat
the spawn pod as hostile (`sp-eha`). On **cloud** (Spawnery-operated, shared infra) this is
non-negotiable; on a **self-hosted** node it's the operator's own box and their call.

## 2. Node class governs the posture

A node is **`cloud`** (Spawnery-operated) or **`self-hosted`** (`NODE_CLASS`, default `cloud`).

| Mechanism | Cloud | Self-hosted |
|---|---|---|
| Egress floor | **always on** (non-disableable, fail-closed) | on by default; disableable (`EGRESS_ENFORCE=false`) |
| Resource limits (cgroups) | on (defaults) | on (defaults); tunable |
| Per-user spawn quota | on (CP-side, applies to all nodes) | on (CP-side) |
| Container runtime | Docker ns; gVisor opt-in where installed | operator's choice |

The class is reported by the node at registration and recorded CP-side (`sp-2as`), stamped on
`spawn_create` telemetry.

## 3. What the floor enforces

### 3.1 Network egress (`sp-rpa`)
- A **per-pod netns firewall** (host `nsenter` + `iptables`) applied **after the sidecar starts and
  before the agent starts** — no window where the untrusted agent runs unfirewalled.
- **Block-floor:** DROP cloud-metadata `169.254.0.0/16` (incl. `169.254.169.254`) + RFC1918
  (`10/8`, `172.16/12`, `192.168/16`); ACCEPT loopback (the agent↔sidecar path) + operator
  `EGRESS_ALLOW_CIDRS`; **default-allow** the public internet otherwise (so the sidecar reaches its
  model upstream, e.g. OpenRouter).
- **Fail-closed** when enforcement is effective: if the firewall can't be applied, the spawn is
  aborted (sidecar stopped), never run unprotected.
- Closes: cloud-metadata credential theft, SSRF / internal-network pivot, RFC1918 exfil.
- **Not** closed by this floor: arbitrary *public* exfil (the stricter app-declared-domain allow-list
  is a later slice). Compensated by audit + trust tiers (§5).

### 3.2 Resource limits — cgroups (`sp-ach`)
Per spawn, on **both** pod containers (sidecar + agent), via Docker `HostConfig.Resources`:
- **memory** (`MEM_LIMIT_MB`, default 1024), **CPU** (`CPU_LIMIT` cores, default 1.0), **pids**
  (`PIDS_LIMIT`, default 256 — fork-bomb guard). Prevents host resource exhaustion by a hostile spawn.

### 3.3 Per-user concurrency quota (`sp-ach`)
CP rejects `CreateSpawn` with `ResourceExhausted` once an owner holds `>= CP_MAX_SPAWNS_PER_OWNER`
(default 5) non-deleted spawns. `0` = unlimited. Prevents one user spawning unboundedly.

### 3.4 Container isolation
- **Baseline:** Docker namespaces (pid/mount/uts/ipc) + the shared netns within a pod (agent joins
  the sidecar's netns so it reaches the sidecar on loopback only).
- **Optional hardening:** `CONTAINER_RUNTIME=runsc` runs the pod under **gVisor** (syscall
  interception) for stronger kernel isolation. **Opt-in, not fail-closed** — gVisor must be installed
  on the host; an absent runtime would break every spawn, so we don't force it.

## 4. Configurables (all knobs)

**Node (spawnlet):**

| Env | Default | Effect |
|---|---|---|
| `NODE_CLASS` | `cloud` | `cloud` = always enforce floor (non-disableable); `self-hosted` = operator's choice. |
| `EGRESS_ENFORCE` | `true` | honored only on self-hosted (cloud forces on). `false` → unrestricted egress (loud warning). |
| `EGRESS_ALLOW_CIDRS` | _(empty)_ | extra CIDRs ACCEPTed before the drops (e.g. a LAN model upstream / DNS resolver). |
| `CONTAINER_RUNTIME` | _(empty)_ | OCI runtime, e.g. `runsc` (gVisor). Empty = Docker default. |
| `MEM_LIMIT_MB` | `1024` | per-spawn memory cap. |
| `CPU_LIMIT` | `1.0` | per-spawn CPU cap (cores). |
| `PIDS_LIMIT` | `256` | per-spawn pids cap. |

**Control plane:**

| Env | Default | Effect |
|---|---|---|
| `CP_MAX_SPAWNS_PER_OWNER` | `5` | per-user concurrent-spawn cap; `0` = unlimited. |

## 5. Deliberately NOT restricted (posture decisions)

- **Agent toolset is allow-all (post-MVP to restrict; `sp-eha` deferred).** Rationale (user decision
  2026-06-01): rather than constrain tools up front, **rely on review/audit to detect abuse** and
  escalate to **VM/microVM isolation in the cloud** if the platform scales. The floor (egress +
  limits + quota) + the layers below contain the blast radius meanwhile.
- **No per-app egress allow-list yet** — only the block-floor (§3.1). App-declared egress domains are
  a later slice.
- **No disk quota** — the demo's Scratch backend is ephemeral (revisit with managed storage, E3).

## 6. Defense-in-depth (how the layers stack)

A misbehaving app/agent must get past, in order:
1. **Trust tier** — `unverified` apps carry the loudest warnings + tightest posture; `scanned`
   (passed the automated App-review scanner, `sp-5a9`); `reviewed` (human). New versions re-enter at
   `scanned`/`unverified`.
2. **Automated App-review scanner** (`sp-5a9`, demo-required) — prompt-injection / exfil / jailbreak
   checks gate open publishing.
3. **Informed consent** at spawn — the user sees what the app declares before running it.
4. **The runtime floor** (this doc) — egress + cgroup limits + quota + (optional) gVisor.
5. **Audit** (E8) — content-aware classification at the sidecar (the one component seeing all model
   traffic). The primary abuse-*detection* mechanism, given allow-all tooling.

No single layer is load-bearing alone — a scanner miss is contained by the floor + consent + audit.

## 7. Verification status (be honest)

- **Hermetic / CI:** firewall rule construction, fail-closed control flow, cgroup-limit + runtime
  mapping (`buildHostConfig`), per-user quota — all unit-tested.
- **Verify-on-host (NOT verified in the dev sandbox — no `iptables`, non-root, no gVisor):** real
  packet drops, real cgroup enforcement, gVisor isolation. These must be validated on a privileged
  node host:
  - egress: `go test -tags egress_e2e ./internal/spawnlet/firewall/ -run TestEgressFloorEnforced`
  - cgroups/gVisor: exercised by a real spawn on a host with the limits/runtime configured.

## 8. Known gaps / roadmap

- VM/microVM isolation for cloud at scale (replaces per-spawn gVisor reliance).
- App-declared per-spawn egress allow-list (tighter than the block-floor).
- Restricted agent toolset (`sp-eha`, post-MVP) if review/audit proves insufficient.
- Disk quotas (with managed storage, E3).
- IPv6 egress rules (metadata is IPv4 today; `sp-rpa` follow-up).
- Scheduler routing by node class (e.g. unverified apps → enforcing cloud nodes only).
- Host-level verification of the egress + cgroup floors on the node host.
