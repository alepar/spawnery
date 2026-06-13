# Fail-Closed Suspend — Design (sp-ei4.1.15)

**Date:** 2026-06-13 · **Bead:** `sp-ei4.1.15` (under epic `sp-ei4.1`) ·
**Basis:** writable-rootfs survival design
([2026-06-12](2026-06-12-writable-rootfs-survival-design.md)) + transient-tier journal
([2026-06-10](2026-06-10-transient-tier-kopia-journal-design.md)). Brainstormed 2026-06-13.

## 1. Problem

On suspend, the node reaps ACP sessions and the manager tears the pod down, then takes the
journal `FinalSnapshot` **after** the destructive `pod.Stop` and treats a snapshot failure as
**non-fatal** (`internal/spawnlet/manager.go` teardown). When the journal sink is unreachable
(observed live: Garage down, `secret-app` with a `durability: node-local` mount), suspend
nukes the scratch dirs and resume restores nothing — **silent data loss**.

Required behavior: when journaling is enabled for the spawn (`journal != nil &&
len(JournalMounts) > 0`) and the final snapshot fails, **abort the suspend** — destroy nothing,
keep the spawn fully ACTIVE and usable, and surface a clear error to the user.

## 2. Core principle

Nothing destructive (session reap, `pod.Stop`, mount-dir finalize) runs until a **durable
journal snapshot exists**. The snapshot is the gate; teardown happens only after it succeeds.
Quiescence during the snapshot comes from **pausing the agent container** (chosen over a live
snapshot, to preserve the roast-M17 "no writes after the final snapshot" guarantee).

## 3. PodBackend `Pause` / `Unpause`

Add to the `PodBackend` interface (`internal/runtime/pod.go`):

```go
Pause(ctx context.Context, h *PodHandle) error
Unpause(ctx context.Context, h *PodHandle) error
```

- **Docker** (`docker_pod.go`): pause/unpause the **agent** container (`h.AgentID`) via new
  `PauseContainer`/`UnpauseContainer` on the `ContainerRuntime` interface + `FakeRuntime`
  (`docker pause`/`docker unpause`). The sidecar stays running; only the agent writes the mounts.
- **CRI/runsc** (`cri/backend.go`): pause/resume the agent container's containerd **task** via
  the existing containerd client seam (from sp-ei4.1.11).
- **Pause failure is non-fatal**: log and proceed to snapshot the live tree (degraded
  quiescence). Only a *snapshot* failure aborts the suspend. This keeps a lane working even if
  pause is unsupported there.

## 4. Manager: split suspend into gate + finish

Today `Suspend`/`SuspendForMigration` call `teardown(capture=true,…)` which does everything.
Split the journaled-suspend path so the node can interleave its session reap:

- **`SnapshotForSuspend(ctx, id) (SuspendResult, error)`** — the gate:
  1. Look up the spawn (do **not** claim/remove from the store yet).
  2. Stop the journal watchers.
  3. `pod.Pause` the agent (non-fatal on error — see §3).
  4. `journal.FinalSnapshot`.
  5. **On failure:** `pod.Unpause`, restart the journal watchers, return the error. The spawn is
     untouched — still in the store, sessions live, running normally.
  6. **On success:** return the per-mount markers; **leave the agent paused** (no writes between
     snapshot and stop). Persist the markers to `journalState` so a resume restores them.
- **`FinishSuspend(ctx, id, …) (SuspendResult, error)`** — the teardown (only after a successful
  gate): claim the spawn → rootfs `CaptureDelta` (the paused container; `docker commit` works
  paused) → `pod.Stop` → remove the egress floor → finalize mount dirs → secrets cleanup →
  journal `Close`. This is the existing teardown body minus the journal snapshot (now done in
  the gate).

`Stop` and `Delete` are unchanged: they keep today's best-effort snapshot and never block
teardown on a flaky journal (terminal intent; fail-closed applies to **suspend only**).

Rootfs-delta scope unchanged: a `CaptureDelta` failure during `FinishSuspend` stays non-fatal
(data mounts are the durable contract; rootfs delta is best-effort), matching today.

## 5. Node: reorder `suspendSpawn` to gate → reap → finish

`internal/node/attach.go` `suspendSpawn`:
1. Call `mgr.SnapshotForSuspend` **first**, while sessions/pumps are still live.
   - **Failure:** emit `SuspendComplete{generation, error: <msg>}` and `Status{ACTIVE}`, then
     return. Nothing reaped; the spawn keeps running with sessions intact.
2. **Success:** reap sessions (`reapSessions` + stop pumps/relays) → `mgr.FinishSuspend` →
   `releaseSlot` → emit `SuspendComplete{markers}` + `Status{SUSPENDED}` (today's success path).

**Reconcile:** an orphaned **paused** managed agent (node died mid-suspend) is reaped on
startup — any snapshot it took is already durable in `journalState`; worst case the spawn
resumes from the base image. (Treated like any other orphan, with the paused state cleaned up.)

## 6. Signal: `error` on `SuspendComplete` (additive proto)

`proto/node/v1/node.proto` — add `string error = N;` to `SuspendComplete` (additive field
number; run `make gen`). On gate failure the node sets it; on success it stays empty.

CP (`internal/cp/server.go` + `lifecycle.go`): the existing `suspendWaiters.deliver` routes the
`SuspendComplete` to the waiting `SuspendSpawn` RPC. When `error` is set, `suspendLocked`:
- leaves the spawn row **ACTIVE** (no transition to SUSPENDED),
- returns a `connect` error (FailedPrecondition) whose message carries the node's detail, e.g.
  *"suspend failed: journal snapshot failed (journal sink unreachable) — spawn left running"*.

## 7. Web: no new UI

`web/src/App.tsx` `onSuspend` already does `try { await suspendSpawn(id) } catch (e) {
toast.error("Suspend failed: " + e.message) }` then `refreshSpawns()`. A failed-suspend RPC
error surfaces as a toast, and because the spawn stays ACTIVE the ledger refresh keeps rendering
it normally. The only requirement is a clear CP error message (§6); no component changes.

## 8. Testing (hermetic)

- **runtime:** `FakeRuntime` records `PauseContainer`/`UnpauseContainer`; assert the Docker/CRI
  backends call them on the right container.
- **manager:** snapshot-failure ⇒ no `pod.Stop`, watchers restarted, spawn still in the store,
  agent paused-then-unpaused, error returned. snapshot-success ⇒ agent paused **before** the
  snapshot and **not** unpaused, `FinishSuspend` calls `Stop` + finalize. `Stop`/`Delete`
  unchanged (best-effort snapshot, always tear down).
- **node:** gate failure ⇒ `SuspendComplete{error}` emitted, sessions **not** reaped, spawn
  stays ACTIVE; gate success ⇒ reap + `FinishSuspend` + `SuspendComplete{markers}` + SUSPENDED.
- **cp:** `SuspendComplete{error}` ⇒ `SuspendSpawn` returns a Connect error, spawn row ACTIVE.

## 9. Out of scope

- Retry/backoff of the failed snapshot (the user retries suspend manually).
- Fail-closed `Stop`/`Delete` (deliberately best-effort).
- A dedicated web "suspend failed" banner beyond the existing toast.
- Cross-node migration interactions (`SuspendForMigration` shares the gate; the rootfs-artifact
  path stays as designed in the writable-rootfs spec).
