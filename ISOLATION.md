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

### 3.1 Network egress (`sp-rpa`, `sp-ff2`)
- Applied on the **host `DOCKER-USER` chain, matched by the pod's bridge source IP** (the sidecar
  owns the netns/IP; the agent shares it), **after the sidecar starts and before the agent starts** —
  no unfirewalled window. Removed on spawn stop (`DOCKER-USER` persists across containers).
- **Why host-side, not in-netns (`sp-ff2`, host-proven):** the original `nsenter`-in-netns floor is a
  **no-op under gVisor/runsc** — netstack does TCP/IP in user space + emits raw frames on the veth,
  so host-kernel netfilter inside the container netns never sees the workload's traffic (0-pkt
  counters; RFC1918 reachable). Host `DOCKER-USER` rules see the veth egress and enforce under
  **both runc and runsc** (verified: metadata + RFC1918 dropped, public reachable).
- **Block-floor (per-pod, `-s <podIP>`):** ACCEPT **DNS (udp/tcp :53)** + operator
  `EGRESS_ALLOW_CIDRS`; then DROP cloud-metadata `169.254.0.0/16` + RFC1918 (`10/8`,`172.16/12`,
  `192.168/16`); **default-allow** the public internet otherwise (so the sidecar reaches its model
  upstream, e.g. OpenRouter). (No loopback rule — agent↔sidecar `127.0.0.1` is never forwarded.)
- **DNS carve-out (`sp-sac`):** `:53` is allowed *before* the RFC1918 drops because resolvers are
  commonly on RFC1918 (a home-server/LAN or cloud internal DNS) — without it the drops break name
  resolution, and the sidecar must resolve its model host. Residual: DNS-tunneling / internal-DNS
  recon is possible; acceptable for the demo (the audit layer sees content; non-:53 RFC1918 stays
  blocked).
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

### 3.4 Container isolation (`sp-s9u`)
- **Baseline:** Docker namespaces (pid/mount/uts/ipc) + the shared netns within a pod (agent joins
  the sidecar's netns so it reaches the sidecar on loopback only).
- **ACP transport (UDS):** the agent's ACP stream rides an in-pod **abstract Unix socket**
  (`@spawnlet-acp`), bridged to `goose acp`'s stdio by a small in-container adapter. The node reaches
  it by entering the pod's network namespace (`setns`) and dialing the socket — so the spawnlet
  process needs **`CAP_SYS_ADMIN`** (in practice: root, already required for the egress floor on cloud
  nodes). This replaces the Docker stdio-attach and is identical across the Docker and CRI backends.
- **Capabilities:** the agent container runs with **`--cap-drop=ALL`** (always; shell work needs none).
- **Read-only rootfs:** `HARDEN_ROOTFS=true` runs the agent with a **read-only rootfs + `/tmp` tmpfs**
  (writes go to the rw `/app/<data>` mounts). Gated/default-off pending per-agent-image validation
  (host-verified that ro-rootfs blocks rootfs writes under runsc).
- **User namespaces:** Docker `userns-remap` is a **daemon-level** setting (`/etc/docker/daemon.json`
  `"userns-remap": "default"`), not a per-container knob — recommended ops config so in-sandbox root
  maps to an unprivileged host UID.
- **Optional gVisor:** `CONTAINER_RUNTIME=runsc` runs the pod under **gVisor** for kernel-attack-
  surface reduction. **Opt-in, not fail-closed** — gVisor must be installed; an absent runtime would
  break every spawn. When set, the spawnlet runs a **startup preflight** (smoke container under the
  runtime) and **exits hard** if it fails — so a misconfigured runsc is caught at boot, not at first
  spawn. (Adopting runsc requires the host-side egress floor in §3.1 — proven necessary.)

## 4. Configurables (all knobs)

**Node (spawnlet):**

| Env | Default | Effect |
|---|---|---|
| `NODE_CLASS` | `cloud` | `cloud` = always enforce floor (non-disableable); `self-hosted` = operator's choice. |
| `EGRESS_ENFORCE` | `true` | honored only on self-hosted (cloud forces on). `false` → unrestricted egress (loud warning). |
| `EGRESS_ALLOW_CIDRS` | _(empty)_ | extra CIDRs ACCEPTed before the drops (e.g. a LAN model upstream / DNS resolver). |
| `CONTAINER_RUNTIME` | _(empty)_ | OCI runtime, e.g. `runsc` (gVisor). Empty = Docker default. When set, validated by a **startup preflight** — spawnlet exits hard if it can't run a smoke container. |
| `MEM_LIMIT_MB` | `1024` | per-spawn memory cap. |
| `CPU_LIMIT` | `1.0` | per-spawn CPU cap (cores). |
| `PIDS_LIMIT` | `256` | per-spawn pids cap. |
| `HARDEN_ROOTFS` | `false` | agent read-only rootfs + `/tmp` tmpfs (default off pending per-image validation). |

Daemon-level (not env): set Docker `"userns-remap"` in `/etc/docker/daemon.json` to remap in-sandbox
root to an unprivileged host UID.

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
- **Host-verified (2026-06-01, privileged dev host):**
  - **Egress floor:** `egress_e2e` PASSES — metadata `169.254.169.254` + RFC1918 `10.0.0.1`
    DROP-confirmed (iptables packet counters + curl blocked), public egress by IP (`1.1.1.1:443`)
    reachable, DNS `:53` carve-out counter-proven to ACCEPT. Run: `just test-egress` (needs Docker +
    iptables + root). Full DNS *resolution* can't be exercised where outbound DNS is environmentally
    blocked — the IP-based check + the unit test cover the floor regardless.
  - **Cgroup limits:** Docker applies `Memory`/`NanoCpus`/`PidsLimit`; kernel cgroup-v2
    `memory.max`/`pids.max`/`cpu.max` match the configured values exactly.
  - **gVisor + the host floor (`runsc` installed, `release-20260525.0`):** the **in-netns floor is a
    proven no-op under runsc** (0-pkt counters, RFC1918 reachable); the **host `DOCKER-USER` floor
    enforces under runsc** (metadata + RFC1918 dropped, public reachable) — the shipped
    `HostFloorApplier` `egress_e2e` PASSES as root. Hardening verified under runsc: `--cap-drop=ALL`,
    `--read-only` blocks rootfs writes (`/tmp` tmpfs writable), and a `true` smoke (the preflight) runs.
- **Still verify-on-host:** a **full real spawn** (goose agent + sidecar) under `runsc` end-to-end
  (image-compat sweep, per the gVisor research) — the primitives are verified; the composed pod is
  not yet. Production gVisor at fleet scale (Phase 2 rollout) and microVMs (Phase 3) remain ahead.

## 8. Known gaps / roadmap

- VM/microVM isolation for cloud at scale (replaces per-spawn gVisor reliance).
- App-declared per-spawn egress allow-list (tighter than the block-floor).
- Restricted agent toolset (`sp-eha`, post-MVP) if review/audit proves insufficient.
- Disk quotas (with managed storage, E3).
- IPv6 egress rules (metadata is IPv4 today; `sp-rpa` follow-up).
- ~~Scheduler routing by node class~~ — **done (`sp-t5p`)**: unverified app versions run only on a
  self-hosted node owned by the author (`registry.PickFor` placement; CreateSpawn gates by tier).
  Remaining: a web UI version-selector so an author can spawn a specific unverified version (today
  the Detail "Spawn" sends no version → latest reviewed).
- Host-level verification of the egress + cgroup floors on the node host.
