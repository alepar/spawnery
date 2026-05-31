# Spawnery — Spawn Lifecycle (Design)

**Status:** Draft v1 (approved in brainstorming; pending user review)
**Date:** 2026-05-31
**Part of:** [System Design](2026-05-26-spawnery-system-design.md) — authoritative for the
**spawn state machine** (the system design §2/§3 commit to "durable instance" + "ephemeral,
scale-to-zero"; this spec makes that lifecycle explicit).
**Depends on:** [E1 Runtime Core](2026-05-27-spawnery-e1-runtime-core-design.md),
[E3 Storage](2026-05-28-spawnery-e3-storage-design.md) (**hard dependency** — see §8),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md).
**Feeds:** the [State/DAO layer](2026-05-31-state-dao-layer-research-brief.md) (the `spawns`
table *is* the CP index; this spec fixes its status enum + fields) and an E0 contracts update
(new lifecycle RPCs — §9).

---

## 1. Why this spec

The system design already frames a **Spawn** as "a private instance binding
`App@version + data repo + model config + personalization + (optional) conversation state`"
(§2) running on a container that is "**ephemeral, scale-to-zero** — woken per session, torn
down on idle" with the CP index holding "`owner → spawns → … status/last-used`" (§3). That is
a durable, resumable resource with an active/suspended lifecycle — but the lifecycle was never
written down, and the current code implements a degenerate one-shot version (create → active →
destroy). This spec defines the explicit state machine, the operations, and the suspend/resume
mechanics, so the CP index (DAO), the contracts, the node agent, and the web UI share one model.

**One-paragraph mental model.** A spawn is a long-lived, owner-private instance of an App. At any
moment it is either **active** (a container is running on some node) or **suspended** (no
container anywhere; its durable state lives in its data backends). Clients **attach/detach** ACP
sessions to an active spawn; opening a suspended spawn transparently **resumes** it. A spawn is
torn down to `suspended` on explicit stop or inactivity, and brought back on demand — its data
(including uncommitted working-tree changes) restored byte-faithfully. The spawn's identity and
config are stable across this cycle; only the container is ephemeral.

---

## 2. States & transitions

Persisted `status ∈ { starting · active · suspending · suspended · error · deleted }`.
"Resume" is operationally identical to "create with an existing config," so it reuses
`starting` (there is no separate `resuming` state).

```
 create ─▶ starting ─▶ active ─(explicit stop | idle timeout)─▶ suspending ─▶ suspended
              │           │                                                      │
            error ◀───────┘ (bring-up / runtime failure)            resume (auto on attach)
              ▲                                                                  │
              └──────────────────────── starting ◀──────────────────────────────┘
 active | suspended ─(explicit delete)─▶ deleted   (terminal; data backend preserved by default)
 CP/node restart: { starting | active (no live route) | suspending } ──reconcile──▶ suspended
```

| Status | Meaning | Kind |
|---|---|---|
| `starting` | a container is being brought up (first create **or** resume) | transient (persisted for crash-recovery) |
| `active` | a container is running on a node | **stable** |
| `suspending` | persisting state + tearing the container down | transient |
| `suspended` | no container anywhere; durable state persisted; resumable | **stable** |
| `error` | a bring-up or runtime transition failed | failure |
| `deleted` | terminal; index entry removed (data backend preserved by default) | terminal |

**Attach/detach is orthogonal** to this machine — it is a property of an `active` spawn (a live
ACP session is present or not), not a spawn state. See §3.

---

## 3. Operations & the client surface

| Operation | Effect |
|---|---|
| **Create** | Provision data backend(s) + write the index row → `starting` → `active`. |
| **Attach** (open in UI) | If `suspended`, **auto-resume** (wake-from-zero), then attach the ACP stream. **Single session**: a second attach by the owner **takes over** and evicts the stale session (friendly for "closed laptop, reopened on phone"). |
| **Detach** | End the ACP session. The container **keeps running** (does *not* suspend) until a timeout fires. |
| **Suspend** | Explicit stop **or** idle timeout (§4): persist state incl. dirty tree (§5) → tear down the container → `suspended`. |
| **Resume** | Re-provision a container (possibly on a different node) with the **same config + backends** → restore data → `active`. Triggered explicitly or implicitly by attach. |
| **Delete** | Evict any container + remove the index entry. **Data backend preserved by default** ("your data is yours"); destroying managed data is an explicit opt-in. |
| **List** | Owner lists all their spawns + current status/last-used (the UI's home surface). |

The web UI shows the list of all spawns with their status (active/suspended), attaches/detaches
ACP sessions on demand, and exposes lifecycle controls (resume/open, suspend, delete).

---

## 4. Inactivity (two-stage, per-node)

Idle detection lives on the **node** (it owns the container and sees relay traffic). Two
**per-node-configurable** thresholds:

- **Detached timeout (short):** no client attached for `T_detached` → suspend.
- **Attached-idle timeout (long):** a client is attached but no activity for `T_idle`
  (`T_idle > T_detached`) → suspend.

**Activity signal:** any relay frame in *either* direction resets the activity clock. The node
observes frame traffic **without parsing ACP** (consistent with the transparent-relay principle),
so "activity" is coarse but correct — a streaming prompt/response keeps the spawn alive; silence
does not. On timeout the node performs a clean suspend (§5) and reports `suspended` to the CP.

---

## 5. Suspend / resume mechanics — **data only**

Resume restores **data**, not conversation. The container process always dies on suspend; the
agent's in-memory ACP session is gone. Conversation continuity (persist + replay transcript) is
**backlog** (`sp` task — §10), gated behind `spawn.yml`'s conversation-history pointer.

"Data only" means the **full working tree**, including **uncommitted** work — suspend must be
invisible. Mechanism (chosen: **Git WIP ref**):

- **On suspend**, the node: `git add -A` (stage tracked + untracked) → **WIP commit under a hidden
  ref `refs/spawnery/suspend/<spawn-id>`** → persist via the **existing** path:
  - **GitHub-native backend:** `git push` the suspend ref.
  - **Blob backend:** `git bundle create --all` already captures *all* refs, so the suspend ref
    rides in the same bundle — **no new storage channel**.
- **On resume**, the node: materialize the backend → **restore the WIP ref into the working tree**
  (reset so the changes reappear as **unstaged** edits) → drop the suspend ref → start a **fresh
  ACP session**.

Rationale: the WIP-ref approach is the only option that reuses *both* persist paths untouched,
stays inside the universal-git substrate (integrity-checked, clonable, diffable), and adds minimal
code. It restores dirty files as unstaged — a non-issue for an agent, which does not depend on a
persistent index across a process death. *Upgrade path:* a `git stash` dual-commit captures the
exact staged/unstaged boundary if that ever matters.

*(Options weighed and rejected: an opaque tar of `/app` — exact but introduces a non-git
side-channel even for git-native backends; stash-precise ref — git-native and byte-exact but more
plumbing than the demo needs.)*

---

## 6. Crash / restart reconciliation

On CP or node restart, spawns left in a transient or orphaned state reconcile to `suspended`:
`{ starting, suspending, active-with-no-live-route }` → `suspended`. A subsequent attach resumes
them normally.

**Honesty caveat (document in-product):** a **clean** suspend always persists the dirty tree (§5)
→ **lossless**. A **hard crash** (node dies without a clean suspend) only has data up to the
storage layer's last **checkpoint** (E3 debounced-persist), so uncommitted work since the last
checkpoint can be lost. *Clean suspend = lossless; crash = lossy-to-last-checkpoint.* This is the
honest contract; tightening it (more frequent checkpoints, WAL) is a later knob.

---

## 7. Identity, ownership & concurrency

- **Stable identity:** the spawn id is constant across the entire active↔suspended cycle. `node_id`
  changes per active episode; placement is decided at each `starting` (so the sidecar knows whether
  to audit — system design §4).
- **Single active container per spawn** at a time. Resume of an already-`active` spawn is a no-op
  attach; there is never more than one live container for a spawn.
- **Single attached session** (MVP), takeover semantics (§3) — consistent with the storage layer's
  single-writer assumption (system design §5).
- **Ownership** is authoritative in the CP index; every lifecycle op is owner-scoped.

---

## 8. Hard dependency — persistent storage

`resume` is meaningless on an ephemeral backend. The current code uses `storage.Scratch`
(seed-on-prepare, **nuke on finalize**). The full lifecycle therefore **requires at least one
persistent backend** — the **E3 managed storage** backend — wired before suspend/resume is real in
the demo. Until then, suspend/resume can be exercised only against a persistent backend stub. This
is a sequencing constraint on the demo MVP, not a design choice.

---

## 9. Downstream ripple (designed when we reach each)

- **State / DAO layer** ([brief](2026-05-31-state-dao-layer-research-brief.md)): the `spawns` table
  **is** the CP index. This spec **fixes its status enum** to §2's set (replacing the earlier
  `stopped`/`lost`) and adds fields: `node_id` (nullable; current active episode), `last_used_at`,
  `suspended_at`, and a `suspend_ref` marker (presence/sha of `refs/spawnery/suspend/<id>`). The
  index stays a **thin pointer** (system design §2): resume-critical config (app@version, model,
  storage bindings) is cached for wake-from-zero, with `spawn.yml` in the data repo remaining the
  authoritative config source.
- **Contracts (E0):** new/changed RPCs — `ListSpawns`, `SuspendSpawn`, `ResumeSpawn`
  (or implicit-on-attach via `Session`), `DeleteSpawn`; `Session` **auto-resumes** a suspended
  spawn; today's `StopSpawn` semantics change from "destroy" to "**suspend**." A separate E0 update
  defines the wire shapes.
- **Node agent (E1):** implement clean-suspend (WIP ref + persist + teardown), resume
  (materialize + restore ref), and the two-stage idle timer.
- **Web client (E6):** the spawn list + status + lifecycle controls + auto-resume-on-open.

---

## 10. Backlog spun out of this design

- **Conversation continuity across suspend/resume** — persist transcript to the data repo per
  `spawn.yml`'s conversation-history pointer; reload on resume. (Deferred; filed as a `sp` task.)
- **Staged/unstaged fidelity** — upgrade the WIP ref to a `git stash` dual-commit if exact index
  state ever matters. (Noted; not filed until needed.)

---

## 11. Success criteria

1. A spawn can be created, attached, detached, explicitly suspended, auto-resumed on re-attach,
   and deleted — surfaced in the web UI's spawn list with correct status throughout.
2. Suspend→resume restores the working tree **including uncommitted edits** (WIP ref), against a
   persistent backend.
3. Idle auto-suspend fires on both thresholds (detached-short, attached-idle-long), per-node
   configurable; relay activity resets the clock.
4. CP/node restart reconciles in-flight/orphaned spawns to `suspended`; they resume cleanly.
5. Delete preserves the data backend by default; destroying managed data is explicit.
6. The CP index reflects the §2 status machine and the §9 fields.
