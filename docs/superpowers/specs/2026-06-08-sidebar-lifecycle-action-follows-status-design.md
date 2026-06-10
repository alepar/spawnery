# Sidebar lifecycle menu follows actual spawn status

**Date:** 2026-06-08
**Mode:** One-shot (Mode B)
**Status:** Approved 2026-06-09 (amended: §3 post-recreate session rebind) — epic sp-ephq

## Problem description

In the web UI left sidebar, each spawn's three-dots (kebab) menu offers a single lifecycle
action that is meant to track the spawn's state: "Suspend" for a running spawn, "Resume" for a
suspended one. Today that item is driven by a *binary* condition in `web/src/shell/Sidebar.tsx`:
`spawn.status === "suspended" ? Resume : Suspend`. Every status that is not exactly `suspended`
falls through to "Suspend". So when a spawn is reaped by the node-failure / inventory reconciler
and correctly transitions to `unreachable` (the red dot the user *does* see), the menu still
offers "Suspend" — which the control plane rejects (`SuspendSpawn` requires `active`), and the
only recovery action ("Recreate") is never offered. The spawn is stranded from the UI. The goal
is for the lifecycle menu item to follow the *actual* status regardless of which event caused the
transition (explicit suspend/resume, node failure, future autosuspend, error).

## Main challenges

The challenge is not detection — the status pipeline is already correct: the CP marks reaped
spawns `unreachable` via `MarkUnreachable` (server.go node-disconnect and `reconcileInventory`
paths), and `ListSpawns` reads the durable status straight from the store, so the web poll already
receives the true status. The real gap is twofold: (1) the UI collapses a 7-value status enum into
a 2-way branch, and (2) the recovery RPC the `unreachable`/`error` states need — `RecreateSpawn`,
which re-provisions a fresh container — already exists on the CP (lifecycle.go) and is wired
through proto/gen (`/cp.v1.SpawnService/RecreateSpawn`), but the web Connect client
(`web/src/api/spawnlet.ts`) never exposed it. So the fix must map each status to the lifecycle
action the CP will actually accept, and wire the already-existing `RecreateSpawn` into the web
client and action bag.

## Key decisions made

This is a **UI-only change** — no backend, proto, or `gen/` changes, because the CP state machine
is already complete (`SuspendSpawn`: active→suspended; `ResumeSpawn`: suspended→active;
`RecreateSpawn`: unreachable|error→active). The menu's action selection becomes a single pure,
unit-tested function `spawnLifecycleAction(status)` so the status→action mapping is the one source
of truth and is exhaustively testable. Transitional states (`starting`, `suspending`) and
`unknown` render a *disabled* item rather than an enabled-but-wrong one, so no menu click can ever
hit a CP precondition failure. Because the mapping is status-driven (not event-driven), a future
autosuspend feature that sets `suspended` is handled automatically with no further UI work.

## Decision points, by section

### 1. Status → action mapping

**Recommended:** a pure function in `web/src/api/spawnlet.ts` (co-located with the `SpawnStatus`
type it switches over):

```ts
export type SpawnLifecycleAction =
  | { kind: "suspend"; label: string }
  | { kind: "resume"; label: string }
  | { kind: "recreate"; label: string }
  | { kind: "pending"; label: string }; // disabled: transitional or unknown

export function spawnLifecycleAction(status: SpawnStatus): SpawnLifecycleAction {
  switch (status) {
    case "active":      return { kind: "suspend",  label: "Suspend" };
    case "suspended":   return { kind: "resume",   label: "Resume" };
    case "unreachable":
    case "error":       return { kind: "recreate", label: "Recreate" };
    case "starting":    return { kind: "pending",  label: "Starting…" };
    case "suspending":  return { kind: "pending",  label: "Suspending…" };
    default:            return { kind: "pending",  label: "Unavailable" }; // unknown
  }
}
```

| status | dot (existing) | menu item | handler | enabled |
|---|---|---|---|---|
| `active` | green | Suspend | `onSuspend` → `SuspendSpawn` | yes |
| `suspended` | grey | Resume | `onResume` → `ResumeSpawn` | yes |
| `unreachable` | red | Recreate | `onRecreate` → `RecreateSpawn` | yes |
| `error` | red | Recreate | `onRecreate` → `RecreateSpawn` | yes |
| `starting` | yellow pulse | Starting… | — | no |
| `suspending` | amber pulse | Suspending… | — | no |
| `unknown` | grey | Unavailable | — | no |

The `recreate` label matches the CP RPC name and the resume semantics (both are non-lossless,
fresh-container operations), keeping UI and backend vocabulary aligned. **Considered:** hiding the
lifecycle item entirely for transitional/unknown states — discarded because a disabled item with a
present-tense label ("Suspending…") communicates *why* there's no action, whereas a vanishing item
reads as a bug. **Considered:** keeping the inline binary ternary in the component — discarded
because it is exactly what produced the bug and is not independently testable.

### 2. Wiring `RecreateSpawn` into the web client

**Recommended:** add to `web/src/api/spawnlet.ts`, mirroring `suspendSpawn`/`resumeSpawn`:

```ts
export async function recreateSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("RecreateSpawn", { spawnId });
}
```

No proto/gen work: `RecreateSpawn` is already a registered Connect procedure
(`gen/cp/v1/cpv1connect/cp.connect.go`), and the web `unary` helper reaches it by name over
Connect-JSON. **Considered:** routing recovery through the existing `onResume` — discarded because
the CP `ResumeSpawn` precondition is `status == suspended`; an `unreachable` spawn would get a
`FailedPrecondition`, which is the very failure this fix removes.

### 3. Action bag + handler (`SpawnActions`, `App.tsx`)

**Recommended:** add `onRecreate: (spawnId: string) => void` to the `SpawnActions` interface in
`Sidebar.tsx`, and implement it in `App.tsx` mirroring `onResume` (call `recreateSpawn`, navigate
to the spawn so the user lands on the recovering view, then `refreshSpawns()`):

```ts
const onRecreate = async (id: string) => {
  try {
    await recreateSpawn(id);
    navigate({ section: "spawn", spawnId: id });
  } catch (e: any) { toast.error("Recreate failed: " + e.message); }
  refreshSpawns();
};
```

**Amendment (2026-06-09, corrected same day):** no extra rebind code is needed after
`recreateSpawn`. Initial concern — navigate-to-same-spawn is a no-op so `bindSpawn` won't re-fire
— turned out to be covered structurally: the spawn's session mirror is dropped with its route
(`router.Drop`), so `ListSessions` returns empty and `SpawnTabs`' 3s roster poll
(`reconcileRoster`) wipes the stale runtimes; recreate then repopulates the roster with fresh
session ids → fresh transcripts. Keep only a **regression test** asserting the dead container's
transcript does not survive a recreate of the currently-selected spawn (guards the roster-poll
mechanism against future store/poll refactors).

Pass `onRecreate` in the `actions={{…}}` bag. Test fixtures that construct an actions object
(`Sidebar.test.tsx`'s `noopActions`, and any actions bag in `App.test.tsx`/`AppShell.test.tsx`)
gain an `onRecreate: vi.fn()` to satisfy the widened interface. **Considered:** making `onRecreate`
optional — discarded; the menu always needs a recovery path for `unreachable`/`error`, and a
required field surfaces any missing wiring at compile time.

### 4. SpawnRow rendering

**Recommended:** replace the binary block at `Sidebar.tsx:138–146` with a single render derived
from `spawnLifecycleAction(spawn.status)`. The `pending` kind renders a `disabled`, muted button
(`text-muted-foreground/50 cursor-default`, no `onClick`); the active kinds render the existing
hover-styled button with test-id `spawn-${kind}-${spawnId}` and dispatch the matching handler,
closing the menu first (as today). Using `spawn-${kind}-…` preserves the existing `spawn-suspend-`
and `spawn-resume-` test-ids and adds `spawn-recreate-` / `spawn-pending-` for free.

### 5. Tests

**Recommended:**
- `web/src/api/spawnlet.test.ts`: exhaustive unit test of `spawnLifecycleAction` over all seven
  statuses (kind + enabled), plus a `recreateSpawn` test matching the existing
  `suspendSpawn`/`resumeSpawn` request-shape assertions.
- `web/src/shell/Sidebar.test.tsx`: extend the kebab test — an `unreachable` spawn (and an `error`
  spawn) shows "Recreate" and calls `onRecreate(id)`; a `suspending`/`starting` spawn shows a
  *disabled* `spawn-pending-…` item that dispatches nothing. Add `onRecreate` to `noopActions`.
- `App.test.tsx` (or equivalent): recreating the *currently selected* spawn does not leave the
  dead container's transcript on screen (per the §3 amendment: the roster poll handles the clear;
  this test guards that mechanism, no new implementation expected).

No new Go/e2e tests: `RecreateSpawn`'s server behavior is already covered in
`internal/cp/lifecycle_test.go`; this change adds no CP logic.

## Scope / non-goals

- **No backend, proto, or `gen/` changes** — `RecreateSpawn` already exists end-to-end.
- **Autosuspend-on-inactivity is not implemented in the CP** and is out of scope here. This design
  is status-driven, so if autosuspend is added later (transitioning to `suspended`), the menu will
  show "Resume" automatically with no further UI work.
- No change to the status dot colors, the Stop/Rename items, or the polling cadence.
