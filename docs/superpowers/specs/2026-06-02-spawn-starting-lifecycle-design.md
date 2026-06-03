# Visible Spawn "Starting" Lifecycle Design

**Date:** 2026-06-02
**Status:** Approved (brainstorming)
**Bead:** sp-wi3

## Problem

`cp.CreateSpawn` blocks in `scheduler.Provision` until the node reports `ACTIVE`, then
returns. So the web never sees a `starting` period: by the time `createSpawn` resolves the
spawn is already active, and the WS connects immediately. We want a visible
**starting → active / error** lifecycle: a yellow dot while starting, green once the node
signals active (then the WS connects), red if it fails. The status is already recorded in
the DB (`store`: `starting` on Create, `active` via `SetActive`, `error` via `SetError`).

## Decisions

- **Async `CreateSpawn`.** Validation stays synchronous; provisioning moves to a background
  goroutine. `CreateSpawn` returns the id immediately with status `starting`.
- **WS gated on `active`.** The web opens the session only when the active spawn's ledger
  status is `active` (green), never during `starting`.
- **Faster poll while starting.** `ListSpawns` poll interval is 1s while any spawn is
  `starting` (snappy green/connect within ~1s of the node signal), else 3s.
- **Failures stay visible.** A spawn that fails to start (`error`) keeps a red dot in the
  sidebar; selecting it shows the header red "error"; the user removes it via the kebab →
  Stop (existing `DeleteSpawn`).
- **"waiting" header state.** A selected `starting` spawn shows the header indicator as
  "waiting" with a grey pulsing dot.

## Architecture

### CP — async `CreateSpawn` (`internal/cp/server.go`)

Keep the synchronous prefix exactly as today: auth, quota, version resolve, declared mounts,
`placementFor` (unverified-non-creator → `PermissionDenied`), mint id, per-spawn lock, name
autogen, `store.Create` (status `starting`). Then **return the id immediately** instead of
provisioning inline. Launch a background goroutine:

```
go func() {
    bg := context.WithoutCancel(reqCtx)      // request ctx is done once CreateSpawn returns
    unlock := s.locks.Lock(spawnID); defer unlock()
    sp, err := s.st.Spawns().Get(bg, spawnID)
    if err != nil || sp.Status != store.Starting { return }  // Stop raced in during the lock gap
    nodeID, perr := s.sched.Provision(bg, spawnID, ver.Ref, model, placement)
    if perr != nil { s.st.Spawns().SetError(bg, spawnID); return }   // log on SetError failure
    if aerr := s.st.Spawns().SetActive(bg, spawnID, nodeID, 1); aerr != nil {
        s.rt.StopOnNode(spawnID); s.rt.Drop(spawnID); s.st.Spawns().SetError(bg, spawnID)
    }
}()
return id
```

- Holding the per-spawn lock across `Provision` serializes Stop/Suspend after provisioning
  (a Stop during `starting` waits, then stops — no interleave). The lock-gap before the
  goroutine acquires it is covered by the `Get`+status re-check.
- On a CP restart mid-`starting`, the existing boot reconcile (`MarkBootUnreachable`, which
  sweeps `{Starting, Active}`) flips the stranded spawn to `unreachable` (red) — no new code.
- Validation errors still return synchronously (no spawn created), so the web's `createSpawn`
  rejects on bad input and resolves with a `starting` id on success.

### Web (`web/src/App.tsx`, `web/src/shell/*`)

- **`spawnApp`**: `await createSpawn(appId, MODEL)` now resolves instantly with the `starting`
  id → `setActiveId(id)`, `conn.waiting()`, `refreshSpawns()` (sidebar shows it yellow),
  **no** `openSession`. The chat input is disabled (not connected).
- **Poll-driven connect** in `refreshSpawns`, after `setSpawns(list)`, for the active spawn
  (`activeIdRef.current`), using a `connRef` mirror to act on transitions only:
  - `active` & `!wsRef.current` → `openSession(activeId)` (→ connecting → connected).
  - `error` (and `connRef !== "error"`) → close any ws + `conn.errored()` (red).
  - `starting` (and `connRef !== "waiting"`) → `conn.waiting()`.
  - vanished from the ledger → existing clear path (`reset()`).
- **`selectSpawn`** branches on the selected spawn's status: `active`→`openSession`;
  `starting`→`waiting()`; `error`→`errored()`; `suspended`→`reset()` (hidden).
- **Faster poll**: the mount effect computes the interval from whether any spawn is
  `starting`: `const ms = list.some(s => s.status === "starting") ? 1000 : 3000;` Implement as
  a self-rescheduling `setTimeout` (so the interval can change) rather than a fixed
  `setInterval`.
- **`useConnStatus`**: add `waiting()` (→ `"waiting"`, clears the slow-timer, no new timer);
  `ConnState` gains `"waiting"`. `closed()`/`reset()` unchanged.
- **`ConnStatus`**: `waiting` → `bg-zinc-400 animate-pulse` + label `"waiting"`.
- **Sidebar** (`Sidebar.tsx` `DOT`): `starting` → `bg-yellow-400 animate-pulse` (yellow, was
  amber). `suspending` stays amber.
- **Chat input gating**: App passes `inputDisabled = busy || conn !== "connected"` to
  `AppShell` → `ChatView` → `PromptInput` (replaces the current `busy`-only `disabled`), so
  the input can't send into a not-yet-connected spawn. The textarea still keeps focus (recent
  fix — only the Send button + the send() guard use `disabled`).

## Testing

- **CP** (`internal/cp`): a `waitActive(t, st, id)` helper that polls the store until
  `status == active` (short timeout). `createActive` calls it after `CreateSpawn`; the
  suspend/resume tests (`TestSuspendSpawn`/`TestResumeSpawn`) wait for active before
  suspending. Add `TestCreateSpawnIsAsyncReturnsStarting`: with a node that does NOT ack,
  `CreateSpawn` returns immediately and `Get` shows `starting`; then ack → becomes `active`.
  Add `TestCreateSpawnProvisionFailureSetsError`: a node that acks ERROR → status `error`.
  Validation-error tests (unknown version, unverified non-creator) still pass synchronously.
- **Web** (vitest): `useConnStatus` gains a `waiting` test; `ConnStatus` renders `waiting`
  (grey pulse + label). The App poll-driven connect/error/waiting transitions are unit-tested
  with a fake `listSpawns` + fake `WebSocket`/`Client` driving a `starting`→`active` sequence
  (assert no `openSession` while starting, `openSession` on active). Sidebar `starting` dot
  test asserts the yellow class.
- **e2e**: the existing specs already wait for the header `"connected"`, which now flows
  `waiting → connecting → connected`; they confirm the async flow end-to-end. The transient
  `starting`/`waiting` state is covered by unit tests (too short/racy to assert reliably in
  e2e with the fast stub).

## Non-goals

- Cancelling an in-flight `starting` provision (Stop waits for it to finish, then stops).
- A spinner/progress bar beyond the dot + "waiting" label.
- Retry-on-failure UI (the user Stops a failed spawn and re-spawns).
