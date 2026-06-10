# Spawnery — Spawn Lifecycle (Design)

**Status:** Draft **v3** (post 2nd adversarial roast — consistency protocol added; pending review)
**Date:** 2026-05-31
**Part of:** [System Design](2026-05-26-spawnery-system-design.md) — authoritative for the
**spawn state machine**.
**Depends on:** [E1 Runtime Core](2026-05-27-spawnery-e1-runtime-core-design.md),
[E3 Storage](2026-05-28-spawnery-e3-storage-design.md) (**hard predecessor**; the suspend persist
path reuses E3's **incremental** bundle/push — §5/§8),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md) (suspend is **per-mount**).
**Feeds:** the [State/DAO layer](2026-05-31-state-dao-layer-design.md) and the contracts predecessor
`sp-mqj` (now incl. **episode generation on every message + node inventory on Register/Heartbeat** —
§6/§9).

> **v3 changelog (2nd roast — the state machine had no DB↔container consistency protocol):** added
> §6 **Consistency model** — episode **generation/fencing**, **reconnect reconciliation against node
> inventory**, **decide-in-DB-then-act + per-spawn lock**, **idempotent generation-keyed commands**,
> and **marker-probed crash recovery**. Node failure no longer auto-suspends: a spawn goes
> **`unreachable`** and recovery is **user-driven** (Recreate / Wait; adopt-if-it-returns; fence the
> old container on recreate). Dirty-tree persist uses **real branches on all git backends incl.
> GitHub** (pollution accepted). Blob persist uses E3's **incremental** bundle (not `--all`). The
> node **binds the mounts the CP sends** (not its own manifest re-parse). Scratch-loss is surfaced at
> **every** resume.

---

## 1. Why this spec

A **Spawn** is a durable, owner-private instance of an App (system design §2) on an **ephemeral,
scale-to-zero** container (§3). This spec defines the explicit lifecycle and — critically — the
protocol that keeps the CP's database (the record of *intent*) consistent with the node's *actual*
running containers, under concurrency and partial failure.

**Mental model.** A spawn is **active** (a container runs on a node) or not-active. Not-active splits
into **suspended** (cleanly torn down, durable state persisted, attach auto-resumes losslessly) and
**unreachable** (its node failed mid-flight; recovery is user-driven). Clients attach/detach ACP
sessions to an active spawn. Identity + config are stable across the cycle; only the container is
ephemeral.

---

## 2. States & transitions

`status ∈ { starting · active · suspending · suspended · unreachable · error · deleted }`.

```
 create ─▶ starting ─▶ active ─(suspend: explicit | idle)─▶ suspending ─▶ suspended
              │           │  ▲                                               │
            error◀────────┤  │ adopt (node returns, gen matches)             │ resume (auto on attach)
              │           │  │                                               │
              │           ▼  │                                               ▼
              │      unreachable ◀─(node deemed failed: stream lost > grace)─ active
              │           │
              │  user acks Recreate (bump generation; fence/stop old container)
              ▼           ▼
        (retry) starting ─▶ active
 {active,suspended,unreachable,error} ─(explicit destroy)─▶ deleted   (terminal; data preserved)
```

| Status | Meaning | Exits |
|---|---|---|
| `starting` | container being brought up (create/resume/recreate) | → `active`, → `error` |
| `active` | container running; CP holds a generation match | → `suspending`, → `unreachable`, → `error`, → `deleted` |
| `suspending` | persisting all mounts + tearing down | → `suspended`, → `error` |
| `suspended` | cleanly down; durable state persisted; **attach auto-resumes** | → `starting`, → `deleted` |
| `unreachable` | node failed while active; container fate unknown | → `active` (adopt), → `starting` (recreate), → `deleted` |
| `error` | a transition failed | → `starting` (retry/recreate), → `deleted` |
| `deleted` | terminal; soft-deleted; data backend preserved | — |

The key change from v2: **a lost node does not auto-flip to `suspended`.** It goes `unreachable`
(§6), because a stream close may be a transient partition (the container is still running and still
writing storage) — silently suspending it and letting attach resume a second container is the
two-writer corruption the roast found.

Attach/detach is **orthogonal** — a property of an `active` spawn, not a state.

---

## 3. Operations & the client surface

**Suspend ≠ Destroy** (cleanup uses Destroy, never Suspend). Every mutating op runs under a
**per-spawn lock** and follows **decide-in-DB-then-act** (§6.3).

| Operation | Effect |
|---|---|
| **Create** | resolve App@version + per-mount backend choices; claim `starting` (gen 1); provision; → `active`. |
| **Attach** (open) | if `suspended`, **auto-resume** then attach; if `unreachable`, present **Recreate / Wait** (no auto-resume). Single session, **takeover** explicitly closes/fences the prior client. Attach cancels a *pending* (not-yet-started) suspend timer. |
| **Detach** | end the session; container keeps running until a timeout (§4). |
| **Suspend** | explicit or idle: claim `suspending`; persist every persistent mount (§5); tear down; → `suspended`. **Once persist has begun it is not cancellable**; an attach arriving mid-suspend **waits for `suspended` then resumes** (never attaches the dying container). Persist failure → `error`. |
| **Resume** | claim `starting` (bump generation) **before** provisioning; re-provision (maybe a new node); restore persistent mounts; → `active`. |
| **Recreate** (user-acked, from `unreachable`/`error`) | bump generation; provision fresh from last persisted/checkpoint state; **the old generation is fenced** (its writes rejected; it is `Stop`ped if its node returns). Marks `recovered` (§6.6). |
| **Destroy / Delete** | claim `deleted` (guarded `WHERE status IN('active','suspended','unreachable','error')`) **first**, then best-effort node `Stop` + route drop. Data backend preserved by default. Rejected while `suspending`. |
| **List** | non-deleted spawns + status/last-used. |

---

## 4. Inactivity (two timers, node-owned, per-node config)

- **Activity signal = node↔agent stdio bytes** (the pod stays after detach, so the node sees agent
  activity whether or not a client is attached; no ACP parsing).
- **Detached timer (short, event-driven):** armed on `SessionClose`, disarmed on `SessionOpen`. No
  reattach within `T_detached` → suspend.
- **Attached-idle timer (long):** while attached, reset by agent-stdio activity. Idle `T_idle`
  (`> T_detached`) → suspend.

Whichever fires first; an attach cancels both. *Known limitation:* a spawn detached while the agent
does long background work is suspended at `T_detached` mid-turn (acceptable for interactive agents).

---

## 5. Suspend / resume mechanics — per-mount, data-only

> **Status note (2026-06-09, sp-a7fs):** the node-side suspend protocol below
> (`CPMessage_Suspend` → quiesce + per-mount persist → `NodeMessage_SuspendComplete` with
> markers) is **deliberately not implemented while storage is Scratch-only** — there is nothing
> to persist, so markers would always be empty and a torn agent write is harmless (the scratch
> volume is discarded on resume regardless). Today's CP suspend is a synchronous hard teardown
> (`StopOnNode` + `Drop` + `SetSuspended`, lifecycle.go), which is correct-by-design under §5's
> scratch row. The proto messages stay reserved; node-side handling lands with the first
> persistent mount backend (sp-u53.1 GitHub). Tracked: sp-a7fs (blocked on sp-u53.1, sp-gd9).

Resume restores **data**, not conversation (fresh ACP session; continuity is backlog §10). Data is
**per mount** (N independently-backed mounts; no single repo). Suspend persists **each mount through
its own E3 backend**; the spawn is `suspended` only if **every persistent mount** persisted (any
failure → `error`). Per-mount completion is recorded **incrementally** as each mount finishes
(DAO `spawn_mounts.persist_marker`), so crash recovery can tell "none done" from "all done, signal
lost" (§6.6).

| Mount backend | Suspend | Resume |
|---|---|---|
| **scratch** | nothing persisted — **non-durable**; contents lost | re-seeded empty |
| **managed / GitHub** (git-native) | WIP commit on branch `spawnery-suspend/<id>/<gen>`, persisted via E3's **incremental** push | clone/fetch + restore WIP, then drop the branch |
| **blob** (`git bundle`) | WIP commit included in E3's **incremental** bundle (not `--all` — E3 §5 `sp-7fj`) | clone from bundle + restore |

**Dirty-tree capture:** stage tracked + non-ignored-untracked (`git add -A`) into a WIP commit on
`spawnery-suspend/<id>/<gen>`. `.gitignore`'d artifacts are **deliberately not persisted**
(regenerable; documented). **GitHub:** the branch is pushed to the user's repo — **pollution
accepted**; users exclude `spawnery-suspend/*` from branch-protection/required-checks (documented
setup note). A **GC pass on every materialize** deletes stale `spawnery-suspend/*` branches (not only
the happy-path resume), so crashes don't accumulate branches.

**Scratch honesty:** whenever a resumed spawn has **any** scratch mount, resume surfaces "the
`<name>` folder is non-durable and was reset" — at **every** resume (clean idle-suspend included),
not only crash recovery. (`secret-app` is all-scratch today, so every idle-suspend resets it.)

**Node binds CP-sent mounts.** The node uses the `{name, backend_uri}` set delivered in `StartSpawn`,
**not** its own manifest re-parse, and validates those names against the manifest at the pinned ref
(mismatch → `error`). This removes the double-source-of-truth (CP `spawn_mounts` vs node
`manifest.Parse`).

---

## 6. Consistency model — DB *intent* ↔ node *truth*

**Source of truth split (state it, don't conflate it):** the **DB is the source of truth for intent
and ownership**; the **node's Register/Heartbeat inventory is ground truth for which containers
actually exist**. Consistency is maintained by five mechanisms.

**The running container is a first-class entity** (DAO `spawn_containers`), separate from the durable
spawn: `generation` + `node_id` identify an *episode* and live on the container row, not the spawn.
**spawn:container = 1-to-0..1**, enforced by a **DB partial-unique index** (`uniq_live_container`) —
so "single active container per spawn" is an invariant the database guarantees, not just app logic
(this is the enforcement the roast demanded). Reconciliation (§6.2) diffs node inventory against the
**live** container rows. (Future: data-backend automerge could make this 1-to-many — relax the
partial unique then.)

### 6.1 Episode generation (fencing)
`spawns.generation` (monotonic, bumped on every `starting` episode — create/resume/recreate). The
generation is threaded through **every** CP→node command (`StartSpawn`/`StopSpawn`/`Suspend`/
`SessionOpen`/`SessionClose`) **and** every node→CP `SpawnStatus`. Rules:
- The CP **drops any `SpawnStatus` whose generation ≠ the row's current generation** (kills stale
  ACTIVE/SUSPENDED from a superseded episode).
- The node **stamps backend writes with the generation**; the backend **fences server-side**
  (compare-and-set: a write from an older generation is rejected). This is what makes a
  partitioned-then-returned old container **harmless** even before the CP can stop it.

### 6.2 Reconnect reconciliation against node inventory
`Register` and `Heartbeat` carry `repeated RunningSpawn{spawn_id, generation, phase}`. On every
(re)connect the CP **diffs the node's inventory against the DB** and acts:
- node lists `(id, gen)` matching a **live container** → **adopt** (rebind the route; no restart). If
  the spawn was `unreachable`, **flip it back to `active`** — this is the Wait→adopt path, and it
  works only because `unreachable` **keeps the live container row** (does not end it).
- node runs `(id, gen)` with **no matching live container** (suspended/deleted/error, or a superseded
  gen after recreate) → **`Stop(id, gen)`** the orphan.
- a **live container** the node does **not** list (and no other node claims) after grace →
  **`unreachable`** (§6.5); **keep the live container row** (fate unknown — ended only on recreate).
A **stream close alone does NOT flip status** — it starts a grace window; only failure-to-reappear
(or a confirming inventory) changes state. This kills the blip→suspend→two-writer bug.

### 6.3 Decide-in-DB-then-act + per-spawn serialization
Every transition **claims its target status with a single guarded `UPDATE` (rowcount=1) before any
node command**, and only the winner sends the command, with the whole `{claim → command → await}`
held under a **per-spawn lock**. Consequences: two concurrent Resumes — only one claims `starting`,
only one provisions (no double container); Delete-vs-Suspend can't interleave (single-statement
guards, no `Get`-then-act TOCTOU). Resume/Create/Recreate **claim `starting` before `provision`.**

### 6.4 Idempotent, generation-keyed commands
`Start`/`Stop`/`Suspend` are idempotent and keyed by `(spawn_id, generation)`. The node **no-ops a
command for a generation it doesn't have**; `Stop` of an absent spawn is **success** (not NotFound).
A retried command is safe; a command for a superseded generation is ignored.

### 6.5 User-driven recovery for `unreachable`
When a node is **deemed failed** (stream lost beyond grace, no reconnect, inventory unconfirmable),
its active spawns go `unreachable` and the **user is notified with one choice: Recreate or Wait.**
- **Wait → node returns** with the spawn running (gen matches): **adopt** → `active`; the Recreate
  offer disappears.
- **User acks Recreate:** bump generation; provision fresh from last persisted/checkpoint state →
  `active` (marked `recovered`, §6.6). The **old generation is fenced** (§6.1) and `Stop`ped if its
  node ever returns (§6.2). No automatic resume happens behind the user's back.

### 6.6 Marker-probed crash recovery
On CP restart, the CP does **not** blindly reconcile; it waits for node inventories (§6.2) within a
grace window, then:
- `suspending` → **probe the per-mount `persist_marker`s**: all present + backend confirms intact →
  `suspended` (clean — the signal was just lost); any missing/torn → `error`.
- `active`/`starting` unclaimed by any reconnected node after grace → `unreachable` (user-driven).
- A spawn recovered from an unclean shutdown is flagged **`recovered`** and resume surfaces "state is
  as of <last checkpoint>; uncommitted work after it may be lost." **`recovered` ≠ a clean CP restart
  that merely lost the in-memory route** (that adopts with no notice).

---

## 7. Identity, ownership & concurrency

- **Stable id** across the lifecycle; `node_id` + `generation` identify the current episode.
- **Single active container per spawn**, enforced by generation fencing (§6.1) + claim-before-provision
  (§6.3) — not merely asserted.
- **Single attached session** with explicit takeover.
- **Ownership** authoritative in the DB, checked once per attach on **both** entry points (gRPC
  `Session` **and** the WebSocket path); the live route is a **projection of `active` rows** —
  rebuilt on adopt/resume, torn on suspend/delete — so a relay never outlives a `deleted`/`suspended`
  row.

---

## 8. Hard predecessor — persistent storage (E3)

Lossless suspend/resume requires a persistent backend (E3 managed), wired before suspend/resume
ships; the demo lifecycle is **gated on E3**. The suspend persist path **reuses E3's incremental
push/bundle**, not full re-uploads. Until E3, the CP state machine is buildable/testable against
scratch, but suspend/resume is not lossless and must not be presented as shipped.

---

## 9. Downstream ripple

- **Contracts (`sp-mqj`, hard predecessor):** `cp.v1` lifecycle RPCs incl. **`RecreateSpawn`**;
  `node.v1` `Suspend` message; **`generation` on `StartSpawn`/`StopSpawn`/`Suspend`/`SessionOpen`/
  `SessionClose` and on `SpawnStatus`**; `StartSpawn` repeated mount field; **`SUSPENDED` phase + a
  node→CP suspend-complete signal carrying per-mount markers**; **`RunningSpawn` inventory on
  `Register`/`Heartbeat`**.
- **State/DAO layer:** `spawns.generation`; status incl. `unreachable`; **per-mount markers on
  `spawn_mounts` (written incrementally)**; status-guarded transitions; per-spawn lock; inventory
  reconciliation; the `RecreateSpawn` path.
- **Node agent (E1):** generation stamping + server-side backend fence; inventory reporting; binds
  CP-sent mounts; per-mount incremental persist/restore + branch GC; the idle timer + takeover fence.
- **Web client (E6):** spawn list + status (incl. **`unreachable` + the Recreate/Wait control**),
  auto-resume-on-open, the scratch-reset + recovered notices.

---

## 10. Backlog

- **Conversation continuity** (`sp-qjy`) — until it ships, auto-resume restores files, not the
  transcript (fine for file-centric apps; a cliff for chat/coach apps — gate broad coach launch).
- **Staged/unstaged fidelity** — `git stash` dual-commit upgrade if exact index state matters.

---

## 11. Success criteria

1. Create / attach / detach / suspend / auto-resume / destroy work with correct status; Destroy is
   distinct from Suspend and is what cleanup uses.
2. Against a persistent backend (E3), suspend→resume restores each persistent mount's tracked tree
   incl. uncommitted edits via the **incremental** path; scratch mounts surface the reset notice.
3. **Partition test:** a transient node stream-drop **adopts** the still-running container on
   reconnect (no second container, no status flip); a real node failure → `unreachable` → user
   **Recreate** provisions a new episode and **fences the old container's writes** when it returns.
4. **Concurrency test:** two concurrent Resumes/Suspends/Deletes on one spawn never start two
   containers and never leave the DB and node disagreeing (decide-then-act + per-spawn lock + gen
   fencing); stale-generation `SpawnStatus` is dropped.
5. CP restart reconciles via node inventory (adopt matches; `unreachable` for unclaimed; `suspending`
   resolved by marker-probe), not blind flips.
6. The DAO reflects §2's machine, `generation`, status-guarded transitions, and per-mount markers.
