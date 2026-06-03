# WebSocket Status Indicator Design

**Date:** 2026-06-02
**Status:** Approved (brainstorming)
**Bead:** sp-qyp

## Problem

The chat header status is free text (`<span data-testid="status">ready</span>` in
`AppShell.tsx`), driven by an ad-hoc `status` string in `App.tsx`. It doesn't clearly
convey the live WebSocket connection state. Replace it with a status **light** (colored
dot) + concise text reflecting the WS lifecycle.

## Decisions

- **Four states**, sourced from the active spawn's WS lifecycle:
  | state | dot | label |
  |---|---|---|
  | `connecting` | grey, pulsing | connecting… |
  | `slow` | amber, pulsing | connecting… |
  | `connected` | green, static | connected |
  | `error` | red, static | error |
- **Hide when there is no live socket.** No spawn open, or a *suspended* spawn selected
  (no socket opens) → the indicator renders nothing (`conn === null`). The sidebar dot
  already conveys suspended.
- **`slow`** = still `connecting` after a ~5s timeout.
- **`connected`** = WS open AND the ACP session is established (matches today's "ready").
- **`error`** = WS `onerror` (or an ACP-init failure). Sticky (red) until the next
  connect/teardown; an `error`→`onclose` sequence does NOT hide the red.

## Architecture

Three small units, each independently testable.

### `useConnStatus` hook — `web/src/shell/useConnStatus.ts`

Isolates the state machine + 5s timer so it's unit-testable without mocking WebSocket.

```ts
export type ConnState = "connecting" | "slow" | "connected" | "error";

export function useConnStatus(slowMs = 5000): {
  conn: ConnState | null;
  connecting(): void; // -> "connecting", arm slowMs timer -> "slow" if still connecting
  connected(): void;  // -> "connected", clear timer
  errored(): void;    // -> "error", clear timer
  closed(): void;     // unexpected ws close: keep "error" if errored, else -> null
  reset(): void;      // intentional teardown: -> null always
}
```

- `connecting()` sets `"connecting"` and arms a timer; on fire, `"connecting"` → `"slow"`
  (a functional `setConn` updater so it only flips if still connecting).
- `connected()`/`errored()` set the state and clear the timer.
- `closed()` preserves a current `"error"` (so error→close keeps red), else `null`.
- `reset()` always `null`.
- The timer is cleared on unmount.

### `ConnStatus` component — `web/src/shell/ConnStatus.tsx`

Pure presentation. `if (!conn) return null;` otherwise:

```tsx
<span data-testid="status" data-status={conn}
      className="flex items-center gap-1.5 text-xs text-muted-foreground">
  <span className={`inline-block h-2 w-2 rounded-full ${DOT[conn]}`} />
  {LABEL[conn]}
</span>
```
with `DOT = { connecting: "bg-zinc-400 animate-pulse", slow: "bg-amber-500 animate-pulse",
connected: "bg-green-500", error: "bg-red-500" }` and `LABEL = { connecting: "connecting…",
slow: "connecting…", connected: "connected", error: "error" }`.

### App + AppShell wiring

`App.tsx`: replace the `status: string` state with `const conn = useConnStatus();` and call
its callbacks at the WS lifecycle points (replacing every `setStatus(...)`):
- `openSession` start → `conn.connecting()`
- ws `onopen` after `initialize`+`newSession` succeed → `conn.connected()`
- ws `onerror` / the ACP-init `catch` → `conn.errored()`
- ws `onclose` (current generation only) → `conn.closed()`
- `closeSession` (switch away / suspend / stop) and `spawnApp`'s create-failure catch
  and selecting a suspended/idle spawn → `conn.reset()` (or `errored()` for create-failure)

`AppShell.tsx`: replace the `status: string` prop with `conn: ConnState | null`; render
`<ConnStatus conn={conn} />` in the header (keeping the header layout). The header title
logic is unchanged.

The free-text strings ("starting…", "ready", "session ended", "suspended", "error: …")
are removed from the header — the dot+label conveys the WS state; the sidebar conveys
ledger status.

## Testing

- **`useConnStatus`** (vitest + `@testing-library/react` `renderHook` + `vi.useFakeTimers`):
  `connecting()` → `"connecting"`; advance `slowMs` → `"slow"`; `connecting()` then
  `connected()` before the timeout → `"connected"` and advancing past the timeout stays
  `"connected"` (timer cleared); `errored()` → `"error"`; `closed()` after `errored()`
  stays `"error"`; `closed()` while connecting → `null`; `reset()` → `null` even from
  `"error"`.
- **`ConnStatus`** (vitest): each of the 4 states renders `data-status` + the right label +
  dot class; `conn === null` renders nothing (`queryByTestId("status")` is null).
- **`AppShell.test`**: updated to the `conn` prop (renders `ConnStatus`; `conn="connected"`
  shows "connected").
- **e2e**: the helpers/specs that assert the header text `"ready"` (chat.spec,
  spawn-lifecycle's `spawnFromMarket`, marketplace.spec) switch to `"connected"` (the
  readiness signal). The rest of the e2e is unchanged and must stay green.

## Non-goals

- A per-spawn connection indicator in the sidebar (the sidebar already has a ledger-status
  dot; this is the header/active-session indicator only).
- Auto-reconnect / retry UI (out of scope).
