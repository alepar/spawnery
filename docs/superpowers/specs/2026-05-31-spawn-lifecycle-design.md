# Spawnery — Spawn Lifecycle (Design)

**Status:** Draft **v2** (post adversarial roast; pending user review)
**Date:** 2026-05-31
**Part of:** [System Design](2026-05-26-spawnery-system-design.md) — authoritative for the
**spawn state machine** (system design §2/§3 commit to "durable instance" + "ephemeral,
scale-to-zero"; this spec makes that lifecycle explicit).
**Depends on:** [E1 Runtime Core](2026-05-27-spawnery-e1-runtime-core-design.md),
[E3 Storage](2026-05-28-spawnery-e3-storage-design.md) (**hard predecessor** — see §8),
[Per-Mount Data Backends](2026-05-29-data-mounts-design.md) (**the suspend mechanism is
per-mount** — see §5).
**Feeds:** the [State/DAO layer](2026-05-31-state-dao-layer-design.md) (the `spawns` table *is*
the CP index) and an E0 contracts update (new lifecycle RPCs + `SUSPENDED` phase + node→CP suspend
signal — §9).

> **v2 changelog (what the roast changed):** suspend is now **per-mount** (the single-`/data`
> WIP-ref model was obsolete under per-mount backends); the dirty-tree capture is scoped to
> tracked + non-ignored files and uses a **real branch** (GitHub rejects custom ref namespaces);
> idle detection uses **agent-stdio activity** + an **event-driven detached timer** (not relay
> frames); **Destroy** is kept distinct from **Suspend** (so cleanup paths don't leak suspended
> spawns); crash recovery is **surfaced, not silent**; the persistent-backend dependency is a
> **hard predecessor**, not a footnote.

---

## 1. Why this spec

The system design already frames a **Spawn** as "a private instance binding
`App@version + data repo + model config + personalization + (optional) conversation state`" (§2)
on a container that is "**ephemeral, scale-to-zero** — woken per session, torn down on idle" with
the CP index holding "`owner → spawns → … status/last-used`" (§3). That is a durable, resumable
resource with an active/suspended lifecycle — but it was never written down, and the current code
implements a degenerate one-shot version (create → active → destroy). This spec defines the
explicit state machine, the operations, and the suspend/resume mechanics, so the CP index (DAO),
the contracts, the node agent, and the web UI share one model.

**Mental model.** A spawn is a long-lived, owner-private instance of an App. At any moment it is
either **active** (a container is running on some node) or **suspended** (no container anywhere;
its durable state lives in its data backends). Clients **attach/detach** ACP sessions to an active
spawn; opening a suspended spawn transparently **resumes** it. A spawn is torn down to `suspended`
on explicit stop or inactivity, and brought back on demand — its **persistent** mounts restored
(including uncommitted work). Its identity and config are stable across the cycle; only the
container is ephemeral.

---

## 2. States & transitions

Persisted `status ∈ { starting · active · suspending · suspended · error · deleted }`. "Resume"
is operationally "start with an existing config," so it reuses `starting`.

```
 create ─▶ starting ─▶ active ─(suspend: explicit stop | idle timeout)─▶ suspending ─▶ suspended
              │           │                                                              │
            error ◀───────┴──── (bring-up / runtime / persist failure)        resume (auto on attach)
              │  ▲                                                                       │
       retry  │  └───────────────────────── starting ◀───────────────────────────────────┘
              ▼
 (error | active | suspended) ─(explicit destroy/delete)─▶ deleted   (terminal; data backend preserved)
 CP/node restart: { starting | active(no live route) } ──reconcile──▶ suspended
                  { suspending } ──reconcile──▶ error   (persist may be torn — see §6)
```

| Status | Meaning | Exits |
|---|---|---|
| `starting` | container being brought up (create **or** resume) | → `active`, → `error` |
| `active` | container running on a node | → `suspending`, → `error`, → `deleted` |
| `suspending` | persisting all mounts + tearing down | → `suspended`, → `error` |
| `suspended` | no container; durable state persisted; resumable | → `starting` (resume), → `deleted` |
| `error` | a transition failed | → `starting` (retry), → `deleted` |
| `deleted` | terminal; index row soft-deleted; data backend preserved by default | — |

Attach/detach is **orthogonal** to this machine — a property of an `active` spawn, not a state (§3).

---

## 3. Operations & the client surface

**Suspend and Destroy are distinct operations** (the roast caught that overloading today's
`StopSpawn` would make every cleanup path — test `defer`, `spawnctl` exit — silently leak suspended
spawns):

| Operation | Effect |
|---|---|
| **Create** | Resolve App@version + per-mount backend choices (§ DAO); provision backends + index row → `starting` → `active`. |
| **Attach** (open) | If `suspended`, **auto-resume** then attach the ACP stream. **Attach atomically cancels any pending suspend timer** on the node. **Single session, takeover:** a second attach by the owner **explicitly closes/errors the prior client and fences its writes** before admitting the new one (no silent orphan, no two-writer window). |
| **Detach** | End the ACP session; the container **keeps running** until a timeout (§4). |
| **Suspend** | Explicit or idle (§4): persist **every persistent mount** (§5) → tear down → `suspended`. A suspend whose persist fails → `error` (not `suspended`). |
| **Resume** | Re-provision a container (possibly a different node) with the same App@version + backends → restore persistent mounts → `active`. |
| **Destroy / Delete** | Tear down any container **without** the suspend-persist, soft-delete the index row. **Data backend preserved by default** ("your data is yours"); destroying managed data is an explicit opt-in. *This is the path test teardown / CLI exit use.* **Delete is rejected while `suspending`** (don't race a half-written persist); it interrupts cleanly from `active`/`suspended`/`error`. |
| **List** | Owner lists all non-deleted spawns + status/last-used (the UI home). |

The web UI shows the spawn list with status, attaches/detaches on demand, and exposes
resume/suspend/destroy controls.

---

## 4. Inactivity (two timers, node-owned, per-node config)

The roast killed "relay-frame-traffic as the activity signal" (detach tears down the relay, so
there are no frames exactly when you need to measure idleness). Corrected model — **two independent
timers on the node**, both per-node-configurable:

- **Activity signal = node↔agent stdio bytes.** The agent container keeps running after a client
  detaches (the pod stays — `router.DetachClient`), and the node relays its stdio, so the node
  observes agent byte-activity **whether or not a client is attached**, without parsing ACP.
- **Detached timer (short, event-driven):** **armed on `SessionClose`**, disarmed on `SessionOpen`.
  No client reattaches within `T_detached` → suspend. (Independent of activity — a detached spawn
  suspends even if the agent is mid-thought.)
- **Attached-idle timer (long):** while a client is attached, reset by agent-stdio activity. No
  activity for `T_idle` (`T_idle > T_detached`) → suspend.

Whichever fires first wins; both are cancelled atomically by an attach (§3). On fire, the node runs
a clean suspend (§5) and reports `SUSPENDED` to the CP.

*Known limitation (documented):* a spawn detached while the agent is doing long background work will
be suspended at `T_detached` mid-turn. Acceptable for interactive agents in the demo; revisit if
background/agentic-long-run workloads land.

---

## 5. Suspend / resume mechanics — **per-mount, data-only**

Resume restores **data**, not conversation (the agent's in-memory ACP session dies; a fresh session
starts — conversation continuity is backlog §10). "Data" is **per mount**, because a spawn has **N
independently-backed mounts** ([data-mounts-design](2026-05-29-data-mounts-design.md)) — there is no
single repo. Suspend persists **each mount through its own backend**; resume materializes each.

**Per-backend suspend behavior:**

| Mount backend | Suspend | Resume |
|---|---|---|
| **scratch** (ephemeral) | **nothing persisted — declared non-durable.** Its contents are lost across suspend. | re-seeded empty (as today). |
| **managed / git-native** (persistent) | capture the dirty tree as a **WIP commit** (see below); persist via the backend's own path | clone/fetch + restore the WIP state |
| **blob** (`git bundle`) | WIP commit included by `git bundle create --all` (carries local refs) | clone from bundle + restore |

**Dirty-tree capture (persistent git mounts):**
- Stage **tracked + non-ignored-untracked** files (`git add -A`) into a **WIP commit**. *Scope note:*
  `.gitignore`'d artifacts (build outputs, `node_modules`, caches) are **deliberately not persisted**
  — they are regenerable and would bloat storage; "invisible resume" means *your tracked working
  state*, not transient junk. This is documented user-visible behavior.
- Store the WIP commit on a **real branch** `refs/heads/spawnery-suspend/<spawn-id>` — **not** a
  custom `refs/spawnery/*` namespace, which **GitHub rejects** (hidden-ref). On the managed backend
  (which Spawnery controls) the branch is kept out of the user's default view; on a user's GitHub it
  is a real (visible) branch, cleaned up on resume.
- On the blob backend, `git bundle create --all` carries the branch automatically (no remote push).

**Resume:** materialize each persistent mount → check out + reset the WIP branch so changes reappear
as **unstaged** edits → delete the WIP branch (and best-effort delete the remote branch on GitHub) →
fresh ACP session. *(Staged/unstaged exactness via a `git stash` dual-commit is the upgrade path if
ever needed — §10.)*

**Coordination:** suspend persists **all** persistent mounts; if **any** mount's persist fails, the
suspend fails → `error` (the spawn is not marked `suspended` on a partial persist). Per-mount persist
state (the branch ref / bundle marker) is recorded in the index per mount (DAO `spawn_mounts`).

---

## 6. Crash / restart reconciliation — surfaced, not silent

On CP/node restart:
- `{ starting, active-with-no-live-route }` → `suspended` (a clean attach later resumes them).
- `{ suspending }` → **`error`**, not optimistically `suspended`: a crash mid-persist may have left a
  **torn** WIP branch / half-uploaded bundle. The spawn is quarantined to `error` until its persist
  state is re-verified (or the user retries), rather than trusting a partial persist on the next
  resume.

**Surfaced data-loss (not a silent footgun):** a spawn reconciled from a crash (no clean suspend ran)
has data only up to the storage layer's last **checkpoint** (E3 debounced-persist). The index marks
such a spawn `recovered`, and **resume surfaces** "recovered from an unclean shutdown — state is as
of <checkpoint time>; uncommitted work after it was lost." Clean suspend = lossless; crash =
lossy-to-last-checkpoint **with an explicit signal**.

---

## 7. Identity, ownership & concurrency

- **Stable identity:** the spawn id is constant across active↔suspended. `node_id` changes per
  active episode; placement is decided at each `starting` (so the sidecar knows whether to audit —
  system design §4).
- **Single active container per spawn**; resume of an already-`active` spawn is a no-op attach.
- **Single attached session** (MVP) with explicit takeover (§3) — consistent with the storage
  single-writer assumption (system design §5).
- **Ownership** is authoritative in the CP index (DB), checked once per attach on **both** client
  entry points (gRPC `Session` **and** the WebSocket path).

---

## 8. Hard predecessor — persistent storage (E3)

`resume` is meaningless on an ephemeral backend; `storage.Scratch` nukes on `Finalize`. **Lossless
suspend/resume therefore requires at least one persistent backend (E3 managed storage), wired
before suspend/resume ships.** Per the demo decision, the **full lifecycle is gated on E3** — the
demo sequences managed storage *before* suspend/resume. Until then the CP-side state machine can be
built and tested (status transitions, list, reconciliation) against scratch, but suspend/resume is
**not lossless** and must not be presented as shipped.

---

## 9. Downstream ripple

- **State / DAO layer** ([design](2026-05-31-state-dao-layer-design.md)): the `spawns` table is the
  CP index — status machine of §2; fields `node_id`, `last_used_at`, `suspended_at`, `recovered`;
  **per-mount persist markers live on `spawn_mounts`** (not a single `suspend_ref`). State
  transitions are **status-guarded** (`UPDATE … WHERE status IN(<valid-from>)`).
- **Contracts (E0):** new RPCs `ListSpawns`, `SuspendSpawn`, `ResumeSpawn`, `DeleteSpawn`; `Session`
  auto-resumes; **`StartSpawn` carries per-mount bindings**; **a new `Suspend` CP→node message**; a
  **`SUSPENDED` `SpawnPhase`** + a node→CP suspend-complete signal (the node currently has no way to
  report suspended). This is a **hard predecessor** of the DAO/lifecycle build, not a sibling.
- **Node agent (E1):** per-mount clean-suspend (WIP branch) + resume (materialize + restore), the
  two-stage idle timer, and the takeover fence.
- **Web client (E6):** spawn list + status + lifecycle controls + auto-resume-on-open + the
  recovered/transcript-gone notices.

---

## 10. Backlog spun out of this design

- **Conversation continuity across suspend/resume** (`sp-qjy`) — persist transcript per `spawn.yml`'s
  pointer; reload on resume. *UX note:* until this ships, auto-resume restores files but **not** the
  chat transcript — acceptable for file-centric seed apps (zork, wiki), but a visible cliff for
  chat/coach apps; gate broad coach launch on it.
- **Staged/unstaged fidelity** — `git stash` dual-commit upgrade if exact index state matters.
- **Crash-recovery verification** — re-verify a torn persist (`suspending`→`error`) instead of
  forcing a manual retry.

---

## 11. Success criteria

1. A spawn can be created, attached, detached, suspended, auto-resumed on re-attach, and destroyed —
   shown in the UI list with correct status; **Destroy is distinct from Suspend** and is what
   cleanup uses.
2. Against a **persistent** backend (E3), suspend→resume restores each persistent mount's tracked
   working tree **including uncommitted edits**; scratch mounts are documented non-durable.
3. Both idle timers fire correctly (detached-short event-driven, attached-idle-long activity-driven),
   per-node configurable; an attach cancels them.
4. CP/node restart reconciles orphans to `suspended` and `suspending` to `error`; recovered spawns
   surface the checkpoint-time data-loss notice.
5. Takeover closes the prior client explicitly with no two-writer window.
6. The CP index reflects §2's machine, status-guarded transitions, and per-mount persist markers.
