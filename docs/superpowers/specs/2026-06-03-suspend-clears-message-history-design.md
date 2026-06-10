# Suspend Clears a Spawn's Message History — Design

**Date:** 2026-06-03
**Status:** Approved, ready for implementation plan
**Scope:** Small UI fix in `web/src/App.tsx`

> **Status (2026-06-09): superseded by architecture** — `sp-skp4` closed. The `buffersRef` /
> App-state mechanism this doc patches was removed in the tabs migration. Clearing now happens
> **structurally**: `SuspendSpawn` → `router.Drop` drops the spawn's session mirror → `ListSessions`
> returns empty → `SpawnTabs`'s roster poll calls `reconcileRoster([])`, which wipes the cached
> runtimes (≤3s, one poll interval) — covering backend-initiated suspends too, which this design's
> UI-event approach could not. Regression coverage: `web/src/sessions/store.test.ts` — "recreate
> cycle: empty roster wipes stale runtimes; repopulated roster starts fresh" asserts the
> empty-roster wipe + fresh-runtime semantics.

## Problem

When a spawn is suspended, its message history lingers in React state:

- `buffersRef` (`Map<spawnId, Item[]>`) keeps the cached transcript for the suspended spawn.
- If the suspended spawn is the currently-selected one, the visible `items` transcript also stays on screen.

A resumed spawn always starts fresh (the agent process is restarted), so the retained
transcript is stale and misleading — e.g. clicking a suspended spawn in the sidebar would
show messages from a session that no longer exists.

## Goal

Suspending a spawn clears its message history from React state, on success.

## Change

Single function: `onSuspend` in `web/src/App.tsx` (currently lines 178–184).

```js
const onSuspend = async (id: string) => {
  try {
    await suspendSpawn(id);
    buffersRef.current.delete(id);
    if (activeIdRef.current === id) { closeSession(); setItems([]); }
  } catch (e: any) { toast.error("Suspend failed: " + e.message); }
  refreshSpawns();
};
```

### Behavior

- **Clear only on success.** Both the buffer delete and the visible-transcript clear run
  *after* the `await suspendSpawn(id)` resolves, inside the `try`. If the API call throws,
  the spawn is still running and its history is still valid, so nothing is cleared.
- **Cached buffer.** `buffersRef.current.delete(id)` drops the cached transcript whether or
  not the spawn was the active one.
- **Active spawn.** When the suspended spawn is the currently-selected one, `setItems([])`
  empties the on-screen transcript immediately, alongside the existing `closeSession()`
  teardown. (Decision: clear immediately rather than waiting until the user navigates away.)
- **Resume path unchanged.** On a later resume, `openSession` → `onHistory` repopulates the
  buffer from the server's fresh replay, so no downstream consumer breaks.

## Approach Chosen

Clear inside `onSuspend` (the action that causes the suspend), rather than reacting to a
`status → suspended` transition in the poll (`refreshSpawns`). Rationale: smallest, most
readable change; co-locates clearing with its cause; matches the intended behavior exactly.

Not covered by this design: a suspend initiated by the backend rather than this UI's button.
If that turns out to be a real scenario, clearing on the status transition in `refreshSpawns`
can be layered on later.

## Testing

Vitest + React Testing Library. Assert that after a successful suspend:

1. The spawn's `buffersRef` entry is gone (e.g. selecting it afterward renders an empty
   transcript).
2. For the active-spawn case, the on-screen transcript renders empty.
3. On a failed suspend (API rejects), the buffer and transcript are left intact.

Implementation follows TDD.
