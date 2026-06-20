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
  **Caveat (roast B):** sharding at *node* granularity is **not bandwidth-aware and has no
  rebalancing primitive** — a single hot node concentrates all its sessions' ciphertext on one CP
  and can recreate the driver-#1 saturation cliff. Bounding per-CP relay bandwidth (and whether any
  node→CP migration is needed) is **spike S5**, not a solved property.
- **Router/pump instantiated on-demand.** A CP (re)binds a spawn's route from durable state + the
  node's (re)connect; it never relies on a pre-existing in-memory route. Restart-safe by
  construction. **Open (roast D):** this covers *client-originated* relay, but **CP-*originated*
  node commands** — provisioning a new spawn on any node, drain, idle-reap-across-owners,
  reschedule — still need the node's in-memory Attach `Sender`, which may live on a peer CP. A
  global DB view yields no Send channel. How background commands actuate a peer-owned node is
  **spike S8** (the deepest open question; the CP→CP mesh was rejected, so an alternative is owed).
- **Scheduler off the DB registry**, with a **slot-reservation/claim** to resolve placement races
  (extends the existing `status_seq` CAS pattern). **Note (roast I):** the reservation must write a
  **separate `reserved_slots` column**, not the node-reported `capacity` field (which the node
  full-overwrites every ~5s and would clobber the reservation — a lost update); effective free =
  reported − reserved. See S4.
- **Pending-op channels:** under affinity, the CP that owns the node serves the spawn's requests
  (post-redirect), so waiter and node stream are usually co-located. **Correction (roast F):** the
  earlier claim that "cross-CP completions already fall back to the DB claim/lease" **overstated the
  code** — only `SuspendComplete` (+ the `SetModel` reconcile loop) has a DB-backed late-delivery
  path today; `Resume`/`Fork`/journal-key completions deliver **only to in-memory waiters on the
  initiating CP**. Ops whose ownership moves mid-flight (a deploy mid-resume) need DB-backed
  reconciliation added — scoped in the deep spec, not assumed present.

### 5.4 Restart / deploy continuity

**Scope note (roast A):** a **CP** restart and a **node/spawnlet** restart are different events with
different continuity stories. mosh's UDP data plane goes **straight to the node** (`terminal.go:30`),
so a CP restart is invisible to a live mosh session; the web chat relay goes *through* the CP, so a
CP restart drops it (→ reconnect + replay below). Keep them separate.

- **mosh CAN survive a spawnlet-daemon restart — re-examined (roast A, then code-verified).** The
  hard limit is narrow: you cannot resurrect a *dead* `mosh-server` (the AES-OCB nonce counter +
  terminal framebuffer are process-local; relaunching on the same port with a saved key = nonce
  reuse, which mosh refuses — Winstein, mosh-devel:
  `https://mailman.mit.edu/pipermail/mosh-devel/2012-September/000307.html`). The spec's *original*
  "reopen the same port with the saved key" wording described exactly that broken path. **But the
  process that matters here usually does not die.** Verified: `mosh-server` runs on the host, binds
  its *own* ephemeral UDP port (the spawnlet is **not** in the UDP datapath — `terminal.go:30`), and
  `mosh-server new` **daemonizes**, so it is already detached from the spawnlet; its child
  `docker exec`s into the agent container (`terminal.go:105-111,182`). A **spawnlet redeploy
  therefore need not touch the live mosh flow** — same host:port, key/nonce/terminal state intact,
  roaming covers any blip. What breaks it today is **our teardown, not the protocol**:
  `gracefulStopAll` reaps the agent container on SIGTERM (`manager.go:744`), and there is **no
  node-local session table**, so the restarted spawnlet forgets the orphaned `mosh-server` and
  `ReapOrphans` (`manager.go:696`) may reap/collide it. **So mosh survives a daemon restart given:**
  (1) **adopt-in-place** — a redeploy reconciles running pods from durable state instead of reaping
  them (the same capability the on-demand router/pump + adopt path needs anyway); (2) **don't
  signal-kill the detached `mosh-server`** on shutdown; (3) a **node-local session table** (the
  process-local sqlite — now correctly justified: not to resume a dead server, but to **re-adopt the
  live one**, avoid port collisions, and let a *dropped* client reattach to the *same* session). It
  dies for real only on **pod recreate / host reboot / `mosh-server` kill** → re-attach to a fresh
  session (tmux-in-container keeps scrollback if the pod survives). Scoped in **spike S3**.
- **Process-local state stays out of shared Postgres.** The principle holds — any genuinely
  per-process node state (not mosh session continuity, which is unrecoverable) belongs in a
  process-local store, not Postgres. This is a design principle, no longer justified by the (false)
  mosh-port-reuse example.
- **Client reconnect.** spawnctl `attach` and the web UI handle reconnect; on reconnect they are
  routed to the (possibly new) owning CP. The redirect mechanism itself is **spike S2** (browser
  WebSockets cannot follow a 307 — it must be an app-level reconnect hint, not an HTTP redirect).
- **Web chat replay-from-last-seen (bounded — roast G).** On WS reconnect the web client requests
  replay from the last frame it saw. This builds on the **node-relay transcript replay**
  (`2026-06-02-node-relay-transcript-replay-design.md`) and the **per-spawn pump**
  (`2026-06-03-spawn-pump-multiclient-design.md`), both **node-side**, so a **CP** restart is
  transparent (new CP rebinds, node replays). **Two limits to design for:** (1) the pump's frame log
  is **bounded** (`pump.go` `defaultMaxLog=2000`, oldest trimmed) — if a deploy outage exceeds the
  buffer, the client falls past `base` and gets a **hard reset, not a replay**; "continuity
  identical to a WS drop" only holds *within* the buffer window. (2) a **node** restart recreates the
  pump at `seq=0` and loses the log + in-memory transcript entirely, breaking last-seen replay. Both
  are **spike S3** (reframed around node restart + buffer retention), not assumed solved.

### 5.5 Open spikes (gate implementation — resolve in the deep spec)

Expanded after the 2026-06-20 roast (REVISE, 15 confirmed). S1–S4 sharpened; S5–S9 added for the
failover/actuation/throughput gaps the roadmap had left unspiked.

- **S1 — Registry write/heartbeat load + read load.** Node heartbeats into Postgres (~5s) × node
  count *plus* per-active-spawn claim heartbeats/`status_seq` bumps, *plus* a synchronized
  deploy **reconnect-storm of discovery-map reads**. Confirm steady write volume, the liveness-window
  query plan, and that a reconnect storm doesn't saturate connections/latency. (Postgres is now on
  the live attach path — see S9.)
- **S2 — Redirect mechanism over streaming transports.** WebSocket/Connect streams don't 307
  cleanly, and **browser WebSockets cannot follow an HTTP redirect at all** — so the mechanism must
  be an **app-level reconnect hint**, not a redirect. Constraints the chosen mechanism MUST meet:
  (a) **must not byte-relay** session traffic (a relaying front-door reinstates the single-choke
  bottleneck + SPOF that multi-CP exists to remove — roast B); (b) **must follow dynamic node→CP
  ownership**, which changes on every CP restart/rolling deploy (commodity LB stickiness is
  static/hash and never consults the live DB map — roast B). Evaluate: app-level reconnect hint vs.
  ownership-aware front-door (control-only) vs. sticky-by-key (likely rejected by (b)).
- **S3 — Node/spawnlet-restart continuity** (reframed — roast G; expanded after mosh re-exam). A
  *CP* restart is transparent (node-side pump); a *node/spawnlet* restart is the real event. Two
  independent sub-questions: **(a) web chat replay** — the pump log is **bounded**
  (`defaultMaxLog=2000`), so an outage past the window forces a **hard reset not a replay**, and a
  spawnlet restart recreates the pump at `seq=0` losing the log + transcript unless adopt-in-place
  rebuilds it or the transcript is made durable; decide retention + acceptable (surfaced, not silent)
  degradation. **(b) mosh survival** — `mosh-server` is daemonized and off the spawnlet datapath, so
  a daemon redeploy can be transparent *if* the deploy path **adopts pods in place** (does not reap
  the agent container), **does not signal-kill** the detached `mosh-server`, and a **node-local
  session table** lets the restarted spawnlet re-adopt the orphan instead of reaping/colliding it
  (`manager.go:696,744`; no terminal registry today — verified). Kill criterion for (b): if a
  spawnlet redeploy cannot be made to leave a live `mosh-server` + its pod untouched, mosh continuity
  across deploys is off the table and clients re-attach.
- **S4 — Placement race + reservation lost-update** (roast I). Verify the slot reservation prevents
  two CPs overfilling a node **given that the node full-overwrites its `capacity` every ~5s**: the
  reservation must live in a **separate `reserved_slots` column / reservations row** so a heartbeat
  can't clobber it; effective free = reported − reserved.
- **S5 — Per-CP relay-bandwidth bound + hot-node skew** (roast B). Does node-ownership affinity
  bound per-CP relay bandwidth, or can a hot node/owner hot-spot one CP? Cheapest test: back-of-
  envelope at beta scale (max nodes/CP × spawns/node × per-session ciphertext rate vs a CP's
  NIC/conn budget) + a one-node-saturated skew scenario. Kill criterion: if a realistic skew
  saturates a CP, affinity needs a rebalancing/migration primitive before it counts as solving
  driver #1.
- **S6 — CP-failover convergence** (roast C). When an owning CP dies/drains, what reassigns its
  nodes' `owning-CP` and forces attached clients off it, and **within what window**? Today nothing
  does until each node's TCP Attach times out (tens of seconds) and re-registers; clients redirected
  in that window hit the dead CP. Prototype: 2 CP + 1 node, kill CP-A with a client attached; measure
  time-to-reachability on CP-B and whether nodes re-home without CP-A's participation.
- **S7 — Node-ownership fence + liveness attribution** (roast C). (a) The `owning-CP` field needs a
  **CAS/lease fence** (mirror the spawn claim lease) or two CPs can both accept a node's Attach
  during a flap and last-writer-wins flaps the route. (b) Node liveness is **CP-proxied** (remote
  spawnlets have no DB creds), so a dead *CP* makes its *healthy* nodes' rows go stale — ORR-6
  auto-reschedule must distinguish "node died" from "node's CP died" or it will **double-run spawns**
  on nodes that are about to re-attach.
- **S8 — Cross-CP node actuation for CP-originated commands** (roast D). Enumerate every
  node-directed call site (`scheduler` `Sender.Send(StartSpawn)`, `router` `node.Send`, idle-reap,
  drain, reschedule); classify client-redirectable vs CP-background-originated. For background ones,
  decide the transport to a peer-owned node: constrain placement to the scheduling CP's own nodes,
  vs. a minimal **control-only** CP→CP channel, vs. schedule-then-handoff. (The full CP→CP byte mesh
  stays rejected; this is control-plane only.)
- **S9 — Postgres on the live path** (roast E). Multi-CP promotes Postgres from durable store to a
  **live-attach/redirect/schedule dependency**, while §3 defers Postgres HA as the accepted SPOF.
  Decide: in-CP cached discovery map with invalidation vs. accept-and-document the dependency; bound
  the blast radius of a Postgres blip on new attaches/redirects/scheduling.

## 6. Workstream decomposition (beads sub-epics under ORR)

Each references this roadmap. IDs assigned at creation.

1. **ORR · Observability** — Prometheus metrics on CP/spawnlet/sidecar; structured leveled logging
   (slog) with correlation/request IDs; `/healthz` + `/readyz` + `/livez` on CP (DB-connectivity
   probe); minimal alerting hooks. *No blockers; foundational — start first.*
2. **ORR · CP reliability** — `signal.NotifyContext` + graceful shutdown on CP (drain in-flight
   RPC/Attach) and sidecar; `recover()` in long-running goroutines; remove the bare
   `panic()` in `internal/sidecar/proxy.go`. *Cheap, high-value; deploy prerequisite.* **Note (roast
   H, partially refuted):** graceful op-completion already releases the spawn claim (`withClaim` →
   `defer claim.Release()` with `context.WithoutCancel`); the residual is an **ungraceful** CP death
   mid-op, which holds the claim for the 30s lease TTL and blocks the new owning CP. Graceful
   shutdown should drain/await in-flight claimed ops; document the ungraceful-death window.
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

- **2026-06-20 — roast (REVISE) folded in.** Adversarial review (9 lenses, 3 domains; 72 raw → 20
  distinct → 15 confirmed, all major-gate, 0 unresolved escalations; same-family Claude panel). The
  affinity+redirect topology held; the roadmap had **oversold three continuity promises and left the
  failover/actuation/throughput unknowns unspiked**. Revisions applied this commit: §5.4 mosh
  continuity claim **corrected to false** (mosh has no server-restart resume; node restart kills the
  session, client re-attaches — Winstein, mosh-devel) and CP-vs-node restart scoped apart; §5.3
  corrected the overstated "cross-CP completions already fall back to DB" claim (only SuspendComplete
  + SetModel-reconcile exist), flagged node-granularity affinity as not bandwidth-aware, and moved the
  slot reservation to a separate column; §5.5 spikes expanded S1–S4 and **added S5–S9** (bandwidth
  bound, failover convergence, ownership fence + liveness attribution, cross-CP actuation, Postgres on
  the live path); §6 ORR-2 noted claim-release-on-drain (roast H, partially refuted). Binding deltas
  carried into the `sp-haj5.4` epic notes. These resolve in the ORR-4 **deep design spec**, which
  re-roasts before implementation.
- **2026-06-20 — mosh continuity re-examined (roast A partially walked back).** Code verification
  showed the roast's "node restart kills mosh, re-attach only" conclusion was too pessimistic for the
  *spawnlet-daemon* restart case: `mosh-server` is daemonized and off the spawnlet UDP datapath
  (`terminal.go:30,182`), so a redeploy can be transparent **if** the deploy path adopts pods in
  place, doesn't signal-kill the detached server, and keeps a node-local session table to re-adopt
  the orphan (today `gracefulStopAll`/`ReapOrphans` defeat this — `manager.go:696,744`). §5.4 and
  spike S3 updated accordingly; mosh dies for real only on pod-recreate / host-reboot / server-kill.
  The nonce-reuse limit (can't resurrect a *dead* server) stands. Net: mosh continuity across
  rolling deploys is achievable and cheap-ish, contingent on adopt-in-place — which the node deploy
  story (ORR-5) needs regardless.
- **2026-06-20 — gating spikes S5/S6/S8 resolved; affinity+redirect validated, no structural
  redesign.** The three highest-risk multi-CP unknowns came back favorable (full findings in the
  `sp-haj5.4.5/.6/.8` bead notes; the spikes are closed):
  - **S5 (bandwidth) — PASS, ~50× headroom at beta.** Correction to driver #1: the model inference
    stream never crosses the CP (provider→in-pod sidecar→agent, in the pod netns) and mosh is
    node-direct; the CP relay carries only web-chat ACP frames for *attached* clients (~5–20 KB/s).
    Worst-case fully-skewed single CP ≈ 40–80 Mbit/s. **No rebalancing primitive needed for beta**;
    add a per-CP relayed-bytes guardrail metric (ORR-1) and file migration as a metric-gated
    follow-up. Net: **driver #2 (HA/restart-survival), not bandwidth, is what justifies multi-CP.**
  - **S6 (failover) — feasible, bounded, but needs active mechanisms.** Load-bearing find: http2
    keepalive PINGs are off on both ends, so black-hole CP death detects via TCP retransmission
    (minutes), not "tens of seconds." Fix is layered: graceful drain = active ownership-release +
    stream close (<2s); ungraceful = http2 `ReadIdleTimeout`/`PingTimeout` (~10–15s) + a CP-liveness
    lease so readers never redirect to a corpse. The http2 keepalive one-liner is a cheap general
    reliability win — **candidate to fold into ORR-2.** Depends on S7's ownership fence.
  - **S8 (cross-CP actuation) — feasible, residual smaller than feared.** Invariant: keep `Sender`
    CP-local (`Sender != nil` ⇔ ownership). Background DB-scan loops get an ownership filter (no new
    transport, co-locates waiters, sidesteps roast F); only synchronous foreign ops (new-spawn
    placement, cross-node fork transfer) need a minimal **control-only `ActuateOnNode` (cmd+ack, not
    bytes)** — does not recreate the bandwidth choke. Evaluator-suspend + orphan-reap are co-located
    by construction (spec/roast were wrong). Remaining spikes S1/S2/S3/S4/S7/S9 resolve in the deep
    spec (`sp-haj5.4.10`).
