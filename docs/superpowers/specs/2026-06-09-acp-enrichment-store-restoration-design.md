# Restore ACP enrichment through the session store

**Date:** 2026-06-09
**Epic:** sp-x8y4 (sp-5x60 folded in)
**Status:** Approved 2026-06-09

## Problem

The multi-session tabs migration (sp-npxq, commit `293d06f`) replaced the App-level ACP frame
reducer with the Zustand session store's `reduceFrame` (`web/src/sessions/store.ts`) — and only
ported half the cases. Everything the ACP-enrichment epic (sp-ufz) shipped *around* that reducer
survived intact:

- **Upstream**: the node pump emits enriched frames — `plan`, `commands`, `mode`, tool payloads
  (`content`/`diff`/`rawInput`/`rawOutput` keyed by `toolId`), and turn frames carrying
  `reason`/`error`/`usage` (`internal/node/pump.go`, `frame.go`).
- **Downstream**: `ChatView` already accepts `commands`, `mode`, `onSetMode`, `onCancel` and
  renders `turnEndLabel`/`usageBadge`; the helpers (`lib/plan.ts`, `lib/mode.ts`,
  `lib/toolChip.ts:upsertTool`, `lib/turn.ts`) exist with passing tests; the upward encoders
  (`encodeSetMode`, `encodeCancel`) exist and the pump handles both control frames.

Only the middle layer is broken: `reduceFrame` drops `plan`/`commands`/`mode` frames entirely,
ignores the `turn` frame's `reason`/`error`/`usage`, appends every `tool` frame as a new item
instead of upserting by `toolId`, and `AcpSessionPanel` feeds ChatView defaults/no-ops instead of
real data and callbacks. Net effect: plans, slash-command menus, mode selection, stop button,
usage badges, stop-reason labels, and tool detail are all invisible/no-ops in the tabs UI despite
working backends.

## Design

This is a restoration, not new design — port semantics from the pre-tabs `applyFrame`
(`git show 293d06f~1` and the sp-ufz commits, e.g. `b47d3e2` for plan), adapted to the per-session
store shape.

### 1. `AcpRuntime` gains session-scoped enrichment state

```ts
export interface AcpRuntime {
  items: Item[];
  turn: TurnState;
  perm: { title: string; reqId: string; options: PermOption[] } | null;
  mode: ModePayload | null;   // NEW: current + available modes
  commands: Command[];        // NEW: agent slash commands
  nextId: number;
  lastSeq: number;
}
```

`EMPTY_RT` gains `mode: null, commands: []`.

### 2. `reduceFrame` restores the dropped cases

- `plan` → replace-in-place single plan `Item` (the `Item` union already has
  `{kind:"plan"; entries}`; use/match the `lib/plan.ts` helper semantics: one plan item, swapped
  wholesale per frame).
- `commands` → `commands = f.cmds ?? []`, replaced wholesale.
- `mode` → merge: the first mode frame (from `session/new`) carries `available`; later frames
  carry only `current`. Keep prior `available` when a frame omits it (`lib/mode.ts` semantics).
- `turn` → also carry `reason`/`error`/`usage` into `TurnState` (the type already declares them);
  a busy frame clears them.
- `tool` → keyed upsert via the existing `upsertTool(items, frame)` (`lib/toolChip.ts`): merge by
  `toolId` when present (update title/status, merge `content`/`diff`/`rawInput`/`rawOutput` from
  `f.tool`), append when no match. Tool frames without `toolId` keep today's append behavior.

### 3. `AcpSessionPanel` wires data + controls

- Pass `commands={rt?.commands}` and `mode={rt?.mode}` to ChatView.
- `onSetMode = (id) => sockRef.current?.send(encodeSetMode(id))`
- `onCancel = () => sockRef.current?.send(encodeCancel())`

No ChatView changes — it already renders all of this when fed.

### 4. Exhaustiveness guard

`reduceFrame`'s switch gets a `never`-typed exhaustiveness check over the server→client subset of
`Frame["kind"]` (client→server kinds `prompt`/`perm_response`/`cancel`/`set_mode` are explicitly
excluded — the store never sees them). This makes the next reducer migration a compile error
instead of a silent feature drop, which is exactly how this regression shipped.

### 5. Tests

- `store.test.ts` (new or extended): one reducer test per restored case — plan replace-in-place,
  commands wholesale replace, mode current-over-available merge, turn reason/error/usage carry +
  clear-on-busy, tool upsert-by-toolId vs append-without-id. Revive assertions from the pre-tabs
  applyFrame tests where they exist in git history.
- `AcpSessionPanel` test: StopButton click sends a `cancel` frame; ModeSelector change sends
  `set_mode`; commands/mode from the store reach ChatView.
- Existing `lib/*` helper tests stay as-is (they never broke).

## Out of scope

- Any node/pump change — the backend side of sp-ufz is complete and tested.
- New enrichment categories beyond what sp-ufz shipped.
- Per-session URL deep-links and other sp-npxq deferred items.
