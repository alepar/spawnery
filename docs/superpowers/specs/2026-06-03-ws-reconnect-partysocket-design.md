# WebSocket Reconnect (partysocket + escalating connect-timeout) Design

**Date:** 2026-06-03
**Status:** Approved (brainstorming)
**Bead:** file at plan time

## Problem

The chat WebSocket has no reconnect and no connect-timeout control (see the prior
investigation): `App.tsx:openSession` does a single `new WebSocket(...)`, relies on the
browser's (long, unconfigurable) connect timeout, and on an unexpected drop the header just
hides — recovery requires the user to switch spawns away and back. We want a maintained,
"boring" reconnect layer with a connect-timeout policy we control.

## Decisions

- **Adopt `partysocket`** (maintained fork of `reconnecting-websocket`, API-compatible with
  the native `WebSocket`) for re-dial + send-buffer + reconnect mechanics. **Pin the exact
  version** (currently `1.1.19`) because we read one of its soft-private fields (below).
- **Escalating per-attempt connect-timeout**, schedule **`500ms → 2.5s → 5s → 15s → 30s`**;
  attempt 5+ stays at 30s, retrying **forever**. (A successful connect resets the chain to
  500ms. The initial connect repeats the first 500ms step once — a known partysocket-timing
  off-by-one we accept; see Architecture → Escalation.)
- **Reuse partysocket's own timeout→reconnect machinery.** `partysocket._connect()` re-reads
  `this._options.connectionTimeout` on *every* attempt (verified in v1.1.19 source: it
  destructures `connectionTimeout` from `this._options` and arms `setTimeout(_handleTimeout,
  connectionTimeout)` each connect). So we set that field to the chain's current step before
  each attempt and let partysocket fire the timeout + reconnect. partysocket's reconnection
  *delay* (`minReconnectionDelay`/`maxReconnectionDelay`) is set near-zero so the escalating
  connect-timeout — not a separate backoff — is the pacing.
- **Reconnect indicator:** on a drop → yellow pulsing **"reconnecting"**; if still down after
  a ~12s grace window → red **"disconnected"** while partysocket keeps retrying silently; on
  the next successful (re)connect → green **"connected"**. Chat input disabled unless
  connected.
- **The ledger poll stays the authority on whether to be connected at all.** partysocket owns
  WS-level reconnect *while the spawn's ledger status is `active`*; the poll still opens the
  socket on `starting→active` and **closes** it (stopping reconnection) on
  `error`/`unreachable`/vanished. So a transient blip → silent reconnect; a genuinely dead
  spawn → torn down, no infinite reconnect.
- **Out of scope:** a post-`open` ACP *handshake* timeout (agent silent after the socket
  opens). The sp-39u readiness probe makes this unlikely (the agent answered `initialize`
  before `active`), so it's a follow-up bead, not bundled here.

## Architecture

### New module: `web/src/shell/reconnectingSocket.ts`

A small class wrapping `PartySocket`, exposing the `WebSocketLike` surface our `Conn` already
needs (`binaryType`, `onmessage`, `send`) plus lifecycle callbacks. One responsibility: a
self-reconnecting socket with the escalating connect-timeout policy.

- **Construction:** `new PartySocket({ url, connectionTimeout: SCHEDULE[0], minReconnectionDelay: ~50, maxReconnectionDelay: ~50, maxRetries: Infinity })`. (`SCHEDULE = [500, 2500, 5000, 15000, 30000]`.)
- **Escalation — success resets, via partysocket's own timeout.** We mutate partysocket's
  per-attempt `connectionTimeout` (which `_connect` re-reads from `_options` each attempt) and
  let its built-in timeout→reconnect fire. Track `step` (index into `SCHEDULE`):
  - Construct with `connectionTimeout: SCHEDULE[0]` (= 500), `step = 0`.
  - On `close` (a failed/timed-out attempt or a drop): `step = min(step + 1, last)`;
    `(socket as any)._options.connectionTimeout = SCHEDULE[step]`.
  - On `open` (**success resets**): `step = 0`; `(socket as any)._options.connectionTimeout = SCHEDULE[0]`.
    A successful connect always resets the next outage to 500ms (no stable/minUptime window —
    even rapid open/drop flapping resets each time, per the explicit decision).
  - **Known off-by-one (accepted decision).** partysocket re-dials *inside* `_handleClose`
    (reading `connectionTimeout` at `_connect`) **before** dispatching the `close` event our
    handler hooks, so our write lands one attempt late. Net *effective* per-attempt timeout:
    the **initial** connect runs `500, 500, 2500, 5000, 15000, 30000, 30000…` (one extra 500);
    **post-success recovery is exact** — `500, 2500, 5000, 15000, 30000…`. Accepted in favor of
    reusing partysocket's timeout machinery + minimal code over an exact initial sequence.
- **Callbacks:** `onOpen()` (fires on *every* (re)connection), `onDown()` (an attempt failed
  or the connection dropped). `close()` calls `partysocket.close()` (permanent — stops
  reconnection) for teardown.
- **Soft-private access:** `connectionTimeout` lives on `partysocket._options`; we write it via
  a typed `as any` cast. The pinned version + a structural guard test (below) protect against
  an upstream rename.

### `web/src/App.tsx` — `openSession` reworked for repeated connects

partysocket fires `open` on every reconnection, so the connection setup that currently runs
once in `ws.onopen` moves into the `onOpen` callback and runs each time:

1. `partysocket.send(JSON.stringify({ spawnId, token }))` — the CP's `HandleWS` expects this
   bind frame first on each new underlying socket.
2. `const c = new Client(socket)` — **recreate the Client** (fresh `Conn` framing buffer +
   cleared `pending` + reset `sessionId`) so a truncated frame from the dropped socket can't
   corrupt the new stream; set `c.onHistory` (the node replays `spawn/history` on each
   (re)connect → transcript restores), `clientRef.current = c`.
3. `await c.initialize(); await c.newSession("/app")` (the repeated-`initialize` path verified
   against goose), then `connected()`.

- `onDown()` → `reconnecting()` (and clears `clientRef`'s pending implicitly via the recreate
  on next open).
- `teardown()` → `socket.close()` (was `ws.close()`), which now also stops reconnection.
- The gen-guard (`genRef`) still wraps the async handshake so a late callback from a superseded
  socket is ignored.
- The ledger poll (`refreshSpawns`/`nextConnAction`) is unchanged except that `error`/
  `unreachable`/vanished must call `teardown()` → `socket.close()` to stop reconnection on a
  dead spawn (it already calls `teardown()` on those transitions; partysocket just makes
  "close" mean "stop retrying").

### `web/src/shell/useConnStatus.ts` — reconnect states

- Add **`reconnecting`** to `ConnState` (yellow pulse, label "reconnecting").
- Add a `reconnecting()` action: set `conn = "reconnecting"` and arm a ~12s grace timer → on
  fire, if still `reconnecting`, set `conn = "error"` (red, label "disconnected"). The socket
  keeps retrying regardless; a subsequent `connected()` clears it to green.
- `connecting()` (initial) and `connected()`/`waiting()`/`errored()`/`reset()` unchanged.
- `ConnStatus.tsx`: `reconnecting → bg-yellow-400 animate-pulse`, label "reconnecting"; the
  red after grace reuses the existing `error` rendering with label "disconnected" (or keep
  "error" — implementer's call, label only).

## Testing

- **`reconnectingSocket` unit tests** (Vitest) with an **injected fake PartySocket** (the
  module takes the constructor as a parameter / factory so tests pass a fake that emits
  `open`/`close` and records writes to `_options.connectionTimeout`). The tests assert the
  values the **wrapper writes** (its contract), not partysocket's internal off-by-one:
  - Constructor writes `500`; successive `close` events write `2500, 5000, 15000, 30000,
    30000…` (`step++`, capped at last).
  - `open` resets the next write to `500` (success resets), including after a flap
    (open → close → open).
  - `close()` stops retries (no further attempts on the fake).
  - (The off-by-one in the *effective* per-attempt schedule is partysocket-internal — a
    documented consequence, not the wrapper's contract — so it isn't asserted here.)
- **Structural guard test:** assert `(new PartySocket({url}) as any)._options.connectionTimeout`
  is a number after construction — fails loudly if an upstream upgrade renames the field
  (the one soft-private dependency).
- **`useConnStatus` test:** `reconnecting → (grace fires) → error`, and `reconnecting →
  connected` clears the grace timer.
- **e2e:** the existing suite still passes (happy path: first connect succeeds fast, one
  `onOpen`, handshake, connected). No new e2e — reconnect timing against real Docker is flaky to assert;
  the unit tests cover the reconnect/escalation logic. (Optionally, later, a node-restart
  recovery e2e — deferred.)

## Non-goals

- Post-`open` ACP handshake timeout (follow-up bead).
- Replacing the ledger poll (it remains the connect/teardown authority; partysocket only owns
  reconnect-while-active).
- A configurable/per-agent schedule (YAGNI; the constant is fine).
