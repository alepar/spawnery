# Production / Operational Readiness Review (ORR) — Roadmap

**Date:** 2026-06-20
**Status:** draft
**Mode:** collaborative (Mode A)
**Umbrella epic:** ORR (see beads)

This is the **decision-of-record** for what "production" means for the spawnery beta and how we
get there. It is a roadmap, not a single-feature spec: it captures the gap analysis, the agreed
beta cut line, and the decomposition into workstreams. Each workstream is its own beads sub-epic
that references this document; the hard one (horizontally-scalable CP) gets its own deep design
spec + adversarial review + spike before implementation.

## 1. Target posture

**Small trusted cloud (beta), operated by us.** A known, invite-only set of users; *we* run the
service. Two consequences reshape the priority order:

- We are on the hook for uptime → **observability** and **reliability** are top priority. A
  service you cannot see and that crashes on SIGTERM is the operator's nightmare.
- Users are trusted, but **workloads are not** — the agent still runs arbitrary LLM-generated
  code. Multi-tenant *isolation* therefore stays load-bearing (and is already our strongest area:
  namespaces, `cap-drop=ALL`, optional gVisor, userns-remap, host-verified egress floor). What we
  *can* defer is multi-tenant *hardening aimed at the user* — RBAC, deep input validation,
  supply-chain signing — because the users won't attack us.

## 2. Gap analysis (grounded, 2026-06-20)

Cross-referenced against the codebase. Already-strong: sandbox isolation; auth core (AS-signed
Ed25519 tokens, GitHub OAuth+PKCE, node mTLS, revocation); data model (SQLite/Postgres + 21 Goose
migrations + per-spawn ownership checks); test depth (325 test files, ~1.4:1, many e2e lanes);
secret custody (owner-sealed HPKE, sidecar-only model key, leak-tested logs).

| # | Gap | State | Headline |
|---|-----|-------|----------|
| 1 | Observability | 🔴 | No metrics (Prometheus/OTEL), no structured logging, no tracing, no alerting. |
| 2 | CP reliability | 🔴 | CP has no signal handling / graceful shutdown; zero panic-recovery in goroutines; a bare `panic()` in sidecar startup. |
| 3 | CP high availability | 🔴 | Documented SPOF (`sp-9um`); single instance, no failover. |
| 4 | Node-failure recovery | 🟡 | Dead node → spawns `UNREACHABLE`, no auto-reschedule; no drain/cordon. |
| 5 | Release/deploy | 🔴 | No backend image publishing, no Go CI build/release, stub web deploy, dev-tagged images, no IaC. *(partly `sp-l17l`)* |
| 6 | Data durability / DR | 🟡 | CP store has no backup (AuthSvc has Litestream, CP doesn't); Garage single-node, well-known dev secrets; no DR runbook. |
| 7 | Secret management | 🟡 | Plaintext `.env`, no rotation, no per-env config. *(managed-keys vault `sp-730k` tracked)* |
| 8 | Multi-tenant hardening | 🟡 | No RBAC, no per-owner rate limiting, no storage quotas, no audit log, client↔CP TLS assumed via external proxy. |
| 9 | Supply chain | 🔴 | Backend images unsigned, no SBOM, no dependency scanning (SPA *is* cosign-signed). |
| 10 | Capacity / scale | 🔴 | Zero benchmarks, no load testing, no admission control beyond free-slot count. |
| 11 | Ops docs | 🟡 | Good deploy/provisioning docs; no runbooks, no DR procedures, no SLO/alerting. |

## 3. Beta cut line

**In scope for beta = P0 + P1 + horizontally-scalable CP.** Deferred items are *filed, not
blocked*.

- **P0:** Observability (#1), CP reliability (#2), prod Postgres + CP-store durability/DR (#3/#6).
- **Multi-CP (spans P0/P1):** horizontally-scalable CP — the agreed answer to the SPOF (#3) *and*
  the relay-bandwidth cliff, *and* the prerequisite for zero-downtime deploys.
- **P1:** Release/deploy + rolling upgrades (#5), node-failure recovery (#4).
- **Deferred (post-beta, P2):** active-active beyond N-CP-on-one-Postgres, **Postgres HA itself**,
  RBAC, supply-chain signing/SBOM/dep-scan (#9), load-testing harness (#10), deep input validation,
  audit logging (#8). Postgres remains the one accepted shared SPOF for beta; its HA is a known,
  separable problem.

## 4. Driver for multi-CP: **both**

Per `sp-9um`, two distinct drivers, both in scope:

1. **Scale under load** — one CP relays *all* session ciphertext; it saturates on bandwidth +
   connection count before compute. We must spread relay load across instances.
2. **Survive process restarts** — *required to deploy new versions at all.* A CP restart must not
   end every conversation; clients reconnect and resume.

## 5. Multi-CP target architecture (decided)

N CP instances behind a load balancer, **one shared Postgres** for durable shared state. Postgres
becomes mandatory (SQLite cannot be shared), which also discharges the store-durability workstream.

### 5.1 What is already multi-CP-friendly
- Each CP process already has a unique `cpID` (`internal/cp/server.go:57`), used as `claim_holder`.
- Transition coordination is DB-arbitrated: `Acquire`/`Heartbeat`/`Release` with `status_seq` CAS
  and a 30s claim lease (`internal/cp/store/spawns.go`). Two CPs cannot both drive one spawn.
- The schema already records `spawn_containers.node_id` — "which node hosts this spawn" is durable.
- Several in-memory caches that would otherwise block multi-CP already have durability epics queued:
  `sp-47ry`/`sp-cmuf` (journal-key store), `sp-g9qt` (device registry), `sp-x4fy` (wire durable
  stores into the CP). Multi-CP **depends on** that work.

### 5.2 What is CP-local today and must change
- **Node attach streams** — a node's bidirectional stream lives on exactly one CP
  (`registry.m[nodeID].Sender`); no cross-CP forwarding (`internal/cp/server.go:310-456`).
- **The registry** — purely in-memory, never persisted; liveness/free-slots invisible to peers
  (`internal/cp/registry/registry.go:36`).
- **The scheduler** — places off the in-memory registry only; two CPs see different node subsets
  (`internal/cp/scheduler/scheduler.go:65`).
- **The router/pump relay** — `router.m[spawnID]` is CP-local; a client on the wrong CP gets
  "unknown spawn" (`internal/cp/router/router.go:103`).
- **Pending-op result channels** (suspend/resume/model/fork waiters) and auth caches (`sessions`,
  `nodeKeyCache`, `pendingIntents`).

### 5.3 The chosen topology: **node-affinity + redirect**

The one real question is *how a client reaches the node stream that lives on a different CP.*
Chosen answer (vs. CP→CP forwarding mesh, vs. a shared message bus):

- **Registry → Postgres.** Nodes heartbeat into a DB table (owning-CP, addr, capacity, liveness,
  class, owner). This gives every CP a consistent scheduling view **and** a node→owning-CP
  discovery map. (Process-*local* data does **not** go here — see 5.4.)
- **Affinity.** Each node attaches to one CP and records ownership in the registry table. A
  client's session is routed/redirected to the CP that owns its spawn's node, so the **live relay
  stays CP-local** and the existing router/pump code is reused unchanged. This is the faithful
  realization of "each CP serves its slice of clients" — the slice is defined by node-ownership.
- **Router/pump instantiated on-demand.** A CP (re)binds a spawn's route from durable state + the
  node's (re)connect; it never relies on a pre-existing in-memory route. Restart-safe by
  construction.
- **Scheduler off the DB registry**, with a **slot-reservation/claim** to resolve placement races
  (extends the existing `status_seq` CAS pattern with a capacity-decrement transaction).
- **Pending-op channels stay valid:** under affinity, the CP that owns the node is the CP that
  serves the spawn's requests (post-redirect), so waiter and node stream are co-located. Cross-CP
  completions fall back to the DB claim/lease + `status_seq`, already in place.

### 5.4 Restart / deploy continuity
- **Process-local state in a process-local sqlite, NOT shared Postgres.** Mosh port assignments /
  session keys persist locally so a node restart reopens the **same ports** and mosh restores
  connectivity automatically. Keeping this out of Postgres avoids polluting shared state with
  per-process data.
- **Client reconnect.** spawnctl `attach` and the web UI handle reconnect; on reconnect they are
  routed to the (possibly new) owning CP.
- **Web chat replay-from-last-seen.** On WS reconnect the web client requests replay from the last
  frame it saw (not a blind full replay), giving continuity identical to any WS drop. This builds
  on the existing **node-relay transcript replay** (`2026-06-02-node-relay-transcript-replay-design.md`)
  and the **per-spawn pump** (`2026-06-03-spawn-pump-multiclient-design.md`), both of which are
  **node-side** and therefore survive a CP restart — the new CP rebinds the route and the node
  replays.

### 5.5 Open spikes (gate implementation — resolve in the deep spec)
- **S1 — Registry write/heartbeat load.** Every node heartbeating into Postgres (~5s) × node count:
  confirm write volume + a liveness-window query plan are fine at beta scale; pick upsert cadence.
- **S2 — Redirect handshake over streaming transports.** WebSocket / Connect streams don't 307
  cleanly. Confirm the exact mechanism: LB-level sticky routing by a spawn/owner key, vs. a
  CP-issued "reconnect to CP-B" hint the client acts on, vs. a thin stateless front-door proxy.
  This is the load-bearing unknown.
- **S3 — Replay-from-last-seen fidelity** across a CP restart: frame sequence numbering must be
  stable/monotonic and node-anchored so "last seen" is meaningful after the route is rebound.
- **S4 — Placement race** under concurrent schedulers: verify the slot-reservation claim prevents
  two CPs overfilling one node.

## 6. Workstream decomposition (beads sub-epics under ORR)

Each references this roadmap. IDs assigned at creation.

1. **ORR · Observability** — Prometheus metrics on CP/spawnlet/sidecar; structured leveled logging
   (slog) with correlation/request IDs; `/healthz` + `/readyz` + `/livez` on CP (DB-connectivity
   probe); minimal alerting hooks. *No blockers; foundational — start first.*
2. **ORR · CP reliability** — `signal.NotifyContext` + graceful shutdown on CP (drain in-flight
   RPC/Attach) and sidecar; `recover()` in long-running goroutines; remove the bare
   `panic()` in `internal/sidecar/proxy.go`. *Cheap, high-value; deploy prerequisite.*
3. **ORR · Prod Postgres + store durability/DR** — Postgres as the prod CP store (already
   supported); backup/PITR; documented restore runbook; prod Garage with real (rotated) secrets,
   not the well-known dev token. *Keystone — blocks multi-CP.*
4. **ORR · Horizontally-scalable CP (multi-instance)** — §5. **Needs its own deep design spec +
   roast + the §5.5 spikes before implementation.** Closes `sp-9um`. *Depends on ORR-3 (Postgres)
   and `sp-47ry` (durable caches).*
5. **ORR · Release/deploy + rolling upgrades** — backend image publishing (CI), non-stub deploy,
   per-env config, blue/green or rolling restart procedure, SLO + client-reconnect behavior
   published. *Depends on ORR-2 (graceful shutdown) + ORR-4 (restart tolerance). Relates `sp-l17l`.*
6. **ORR · Node-failure recovery** — auto-reschedule (or surfaced one-click recreate) for spawns on
   a dead node; `drain`/`cordon` node command; node lifecycle management. *Depends on ORR-4's
   DB-backed registry.*
7. **ORR · Deferred hardening (post-beta)** — P2 tracking epic: RBAC, supply-chain signing/SBOM/dep
   scanning, load-testing harness, deep input validation, audit logging, Postgres HA. *Filed, not
   blocked for beta.*

## 7. Sequencing

```
ORR-3 (prod Postgres) ──┬──> ORR-4 (multi-CP) ──┬──> ORR-5 (deploy/rolling)
                        │      ▲                 └──> ORR-6 (node recovery)
sp-47ry (durable caches)┴──────┘
ORR-2 (reliability) ───────────────────────────────> ORR-5
ORR-1 (observability) — parallel, no blockers, start now
ORR-7 (deferred) — file, revisit post-beta
```

Recommended first moves: **ORR-1 (observability)** and **ORR-2 (reliability)** in parallel
immediately (independent, cheap, and prerequisites for operating + deploying), **ORR-3 (prod
Postgres)** next as the keystone, then the **ORR-4 deep spec + spikes** before any multi-CP code.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
