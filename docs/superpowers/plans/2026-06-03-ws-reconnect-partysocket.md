# WebSocket Reconnect (partysocket) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The chat WebSocket auto-reconnects forever with an escalating per-attempt connect-timeout (`500ms → 2.5s → 5s → 15s → 30s`, success resets), and the header shows a "reconnecting" → "disconnected" indicator.

**Architecture:** A new `reconnectingSocket.ts` wraps `partysocket` as a `WebSocketLike` the existing `acp/Conn` already consumes; it escalates the connect-timeout by mutating partysocket's per-attempt `_options.connectionTimeout` on each `close` and resetting on `open`. `App.tsx:openSession` constructs it instead of a raw `WebSocket` and re-runs the ACP handshake on every (re)connection; the ledger poll still owns connect/teardown while partysocket owns WS-level reconnect. `useConnStatus` gains `reconnecting`/`disconnected` states.

**Tech Stack:** React 19 + Vite + Vitest + TS; `partysocket` (pinned exact, `1.1.19`); existing `web/src/acp/{conn,client}.ts` framing.

**Bead:** sp-yry. **Spec:** `docs/superpowers/specs/2026-06-03-ws-reconnect-partysocket-design.md`.

**Conventions:** commit `--no-verify` (beads hook), local-only repo (NO push). Hermetic Vitest for the implementer; the Playwright e2e (real Docker) is orchestrator-run. Run web commands from `web/`.

---

## File Structure

- **Create `web/src/shell/reconnectingSocket.ts`** — the partysocket wrapper: a `WebSocketLike`
  (so `acp/Conn` is unchanged) that auto-reconnects with the escalating connect-timeout, firing
  `onOpen`/`onDown` callbacks; `close()` stops reconnection. The PartySocket constructor is
  injectable (`makeSocket`) for hermetic tests. Holds the only soft-private dependency
  (`_options.connectionTimeout`).
- **Create `web/src/shell/reconnectingSocket.test.ts`** — unit tests with a fake socket.
- **Modify `web/src/shell/useConnStatus.ts`** — add `reconnecting`/`disconnected` states + a
  `reconnecting()` action with a grace timer.
- **Modify `web/src/shell/ConnStatus.tsx`** — render the two new states.
- **Modify `web/src/shell/useConnStatus.test.ts`** — test the grace transition.
- **Modify `web/src/App.tsx`** — `openSession` uses `ReconnectingSocket`; handshake on each
  `onOpen`; `onDown → reconnecting()`; `teardown` closes it.

---

## Task 1: partysocket wrapper (`reconnectingSocket.ts`) + unit tests

**Files:**
- Create: `web/src/shell/reconnectingSocket.ts`
- Test: `web/src/shell/reconnectingSocket.test.ts`

- [ ] **Step 1: Install partysocket (pinned exact)**

Run (from `web/`): `npm install --save-exact partysocket@1.1.19`
Expected: `package.json` `dependencies` gains `"partysocket": "1.1.19"` (no caret). Confirm the
named export with: `node -e "console.log(Object.keys(require('partysocket')))"` — expect it to
include `WebSocket`. (If the class is only the default export, adjust the import in Step 3 to
`import PartySocket from "partysocket/ws"` accordingly.)

- [ ] **Step 2: Write the failing tests**

Create `web/src/shell/reconnectingSocket.test.ts`:

```ts
import { describe, it, expect, vi } from "vitest";
import { ReconnectingSocket, SCHEDULE, type PartySocketLike, type MakeSocket } from "./reconnectingSocket";

// A controllable fake standing in for a PartySocket instance.
class FakeSocket implements PartySocketLike {
  binaryType = "blob";
  onmessage: ((ev: { data: any }) => void) | null = null;
  closed = false;
  _options: { connectionTimeout?: number };
  sent: any[] = [];
  private listeners: Record<string, ((ev: any) => void)[]> = { open: [], close: [], message: [] };
  constructor(public url: string, public options: any) {
    this._options = { connectionTimeout: options.connectionTimeout };
  }
  send(data: any) { this.sent.push(data); }
  close() { this.closed = true; }
  addEventListener(type: "open" | "close" | "message", cb: (ev: any) => void) { this.listeners[type].push(cb); }
  emit(type: "open" | "close" | "message", ev?: any) { this.listeners[type].forEach((cb) => cb(ev)); }
}

function setup() {
  let fake: FakeSocket | undefined;
  const make: MakeSocket = (url, options) => (fake = new FakeSocket(url, options));
  const onOpen = vi.fn();
  const onDown = vi.fn();
  const rs = new ReconnectingSocket("ws://x/ws", { onOpen, onDown, makeSocket: make });
  return { rs, fake: fake!, onOpen, onDown };
}

describe("ReconnectingSocket", () => {
  it("starts with the first schedule step and Infinity retries", () => {
    const { fake } = setup();
    expect(fake._options.connectionTimeout).toBe(SCHEDULE[0]); // 500
    expect(fake.options.maxRetries).toBe(Infinity);
  });

  it("escalates connectionTimeout on each close, capping at the last step", () => {
    const { fake, onDown } = setup();
    const got: number[] = [];
    for (let i = 0; i < SCHEDULE.length + 1; i++) {
      fake.emit("close");
      got.push(fake._options.connectionTimeout!);
    }
    // step++ on each close: 2500, 5000, 15000, 30000, then capped at 30000.
    expect(got).toEqual([2500, 5000, 15000, 30000, 30000, 30000]);
    expect(onDown).toHaveBeenCalledTimes(SCHEDULE.length + 1);
  });

  it("resets to the first step on open (success resets)", () => {
    const { fake, onOpen } = setup();
    fake.emit("close"); // -> 2500
    fake.emit("close"); // -> 5000
    fake.emit("open");
    expect(fake._options.connectionTimeout).toBe(SCHEDULE[0]); // 500
    expect(onOpen).toHaveBeenCalledTimes(1);
    fake.emit("close"); // first failure of the new chain -> 2500 again
    expect(fake._options.connectionTimeout).toBe(2500);
  });

  it("forwards messages to onmessage and sends through the socket", () => {
    const { rs, fake } = setup();
    const got: any[] = [];
    rs.onmessage = (ev) => got.push(ev.data);
    fake.emit("message", { data: "hello" });
    expect(got).toEqual(["hello"]);
    rs.send("bind");
    expect(fake.sent).toEqual(["bind"]);
  });

  it("close() stops reconnection", () => {
    const { rs, fake } = setup();
    rs.close();
    expect(fake.closed).toBe(true);
  });
});

describe("partysocket structural guard", () => {
  it("real PartySocket exposes _options.connectionTimeout (pinned-version contract)", async () => {
    const { WebSocket: PartySocket } = await import("partysocket");
    const ps: any = new PartySocket("ws://localhost:1", [], { connectionTimeout: 500, startClosed: true });
    expect(typeof ps._options.connectionTimeout).toBe("number");
    ps.close();
  });
});
```

- [ ] **Step 3: Run the tests to verify they fail**

Run (from `web/`): `npx vitest run src/shell/reconnectingSocket.test.ts`
Expected: FAIL — `reconnectingSocket.ts` does not exist / `ReconnectingSocket` undefined.

- [ ] **Step 4: Write `web/src/shell/reconnectingSocket.ts`**

```ts
import { WebSocket as PartySocket } from "partysocket";
import type { WebSocketLike } from "@/acp/conn";

// SCHEDULE: per-attempt connect-timeout (ms); the last value repeats forever; a successful connect
// resets to SCHEDULE[0]. NOTE: partysocket re-dials inside _handleClose (reading connectionTimeout)
// BEFORE dispatching the `close` event we hook, so the *effective* initial-connect sequence repeats
// 500 once (500,500,2500,5000,15000,30000,…); post-success recovery is exact. Accepted (see spec).
export const SCHEDULE = [500, 2500, 5000, 15000, 30000];

// The subset of a PartySocket instance the wrapper drives. `_options` is partysocket's soft-private
// options bag; we mutate `connectionTimeout` on it — the only internals dependency, guarded by the
// pinned version + the structural test in reconnectingSocket.test.ts.
export interface PartySocketLike {
  binaryType: string;
  onmessage: ((ev: { data: any }) => void) | null;
  send(data: any): void;
  close(): void;
  addEventListener(type: "open" | "close" | "message", cb: (ev: any) => void): void;
  _options: { connectionTimeout?: number };
}

export type MakeSocket = (url: string, options: Record<string, any>) => PartySocketLike;

const defaultMake: MakeSocket = (url, options) =>
  new PartySocket(url, [], options) as unknown as PartySocketLike;

export interface ReconnectingOpts {
  onOpen: () => void;       // fires on EVERY (re)connection — caller re-runs the ACP handshake
  onDown: () => void;       // fires on every failed attempt / drop
  makeSocket?: MakeSocket;  // injected in tests
}

// ReconnectingSocket wraps a PartySocket as the WebSocketLike that acp/Conn consumes. It reconnects
// forever; each attempt's connect-timeout walks SCHEDULE (escalate on close, reset on open). The
// reconnection *delay* is pinned near-zero so the escalating connect-timeout is the sole pacing.
export class ReconnectingSocket implements WebSocketLike {
  private ps: PartySocketLike;
  private step = 0;
  onmessage: ((ev: { data: any }) => void) | null = null;

  constructor(url: string, opts: ReconnectingOpts) {
    const make = opts.makeSocket ?? defaultMake;
    this.ps = make(url, {
      connectionTimeout: SCHEDULE[0],
      minReconnectionDelay: 50,
      maxReconnectionDelay: 50,
      maxRetries: Infinity,
    });
    this.ps.addEventListener("message", (ev) => this.onmessage?.(ev));
    this.ps.addEventListener("open", () => {
      this.step = 0;
      this.ps._options.connectionTimeout = SCHEDULE[0]; // success resets the chain
      opts.onOpen();
    });
    this.ps.addEventListener("close", () => {
      this.step = Math.min(this.step + 1, SCHEDULE.length - 1);
      this.ps._options.connectionTimeout = SCHEDULE[this.step]; // escalate the next attempt
      opts.onDown();
    });
  }

  get binaryType() { return this.ps.binaryType; }
  set binaryType(v: string) { this.ps.binaryType = v; }
  send(data: string | ArrayBufferView) { this.ps.send(data); }
  close() { this.ps.close(); }
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run (from `web/`): `npx vitest run src/shell/reconnectingSocket.test.ts`
Expected: PASS (6 tests). If the structural-guard test errors because jsdom lacks a global
`WebSocket`, that indicates a real environment gap — do NOT stub it away; report it (the e2e/app
relies on the browser `WebSocket`, and Vitest's jsdom provides one).

- [ ] **Step 6: Commit**

```bash
git add web/package.json web/package-lock.json web/src/shell/reconnectingSocket.ts web/src/shell/reconnectingSocket.test.ts
git commit --no-verify -m "feat(web): partysocket reconnecting socket with escalating connect-timeout [sp-yry]"
```

---

## Task 2: `useConnStatus` + `ConnStatus` — reconnecting / disconnected states

**Files:**
- Modify: `web/src/shell/useConnStatus.ts`
- Modify: `web/src/shell/ConnStatus.tsx`
- Test: `web/src/shell/useConnStatus.test.ts`

- [ ] **Step 1: Write the failing test**

Add to `web/src/shell/useConnStatus.test.ts` (mirror the existing tests' style — they use
`@testing-library/react`'s `renderHook` + `act`):

```ts
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useConnStatus } from "./useConnStatus";

describe("useConnStatus reconnecting", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("reconnecting -> disconnected after the grace window", () => {
    const { result } = renderHook(() => useConnStatus(5000, 12000));
    act(() => result.current.reconnecting());
    expect(result.current.conn).toBe("reconnecting");
    act(() => vi.advanceTimersByTime(12000));
    expect(result.current.conn).toBe("disconnected");
  });

  it("connected() during the grace window cancels the -> disconnected transition", () => {
    const { result } = renderHook(() => useConnStatus(5000, 12000));
    act(() => result.current.reconnecting());
    act(() => result.current.connected());
    act(() => vi.advanceTimersByTime(12000));
    expect(result.current.conn).toBe("connected");
  });
});
```

- [ ] **Step 2: Run it to verify it fails**

Run (from `web/`): `npx vitest run src/shell/useConnStatus.test.ts`
Expected: FAIL — `result.current.reconnecting` is not a function / `useConnStatus` takes one arg.

- [ ] **Step 3: Update `web/src/shell/useConnStatus.ts`**

Add the two states to the union, add a `graceMs` param, and add the `reconnecting` action; return
it. Full updated file:

```ts
import { useCallback, useEffect, useRef, useState } from "react";

export type ConnState =
  | "waiting" | "connecting" | "reconnecting" | "slow" | "connected" | "error" | "disconnected";

// useConnStatus is the WS connection-status state machine for the chat-header indicator. `conn` is
// null when there is no live socket (the indicator is hidden). connecting() arms a `slowMs` timer
// that flips connecting -> slow if still connecting when it fires. reconnecting() arms a `graceMs`
// timer that flips reconnecting -> disconnected (red) if the socket hasn't recovered by then (the
// socket keeps retrying regardless).
export function useConnStatus(slowMs = 5000, graceMs = 12000) {
  const [conn, setConn] = useState<ConnState | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const clearTimer = useCallback(() => {
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
  }, []);

  const connecting = useCallback(() => {
    clearTimer();
    setConn("connecting");
    timer.current = setTimeout(() => {
      setConn((c) => (c === "connecting" ? "slow" : c));
    }, slowMs);
  }, [clearTimer, slowMs]);

  // The socket dropped and is retrying: yellow "reconnecting", then red "disconnected" after grace.
  const reconnecting = useCallback(() => {
    clearTimer();
    setConn("reconnecting");
    timer.current = setTimeout(() => {
      setConn((c) => (c === "reconnecting" ? "disconnected" : c));
    }, graceMs);
  }, [clearTimer, graceMs]);

  // The selected spawn is still starting (no socket yet) — grey-pulse "waiting" until it goes active.
  const waiting = useCallback(() => { clearTimer(); setConn("waiting"); }, [clearTimer]);
  const connected = useCallback(() => { clearTimer(); setConn("connected"); }, [clearTimer]);
  const errored = useCallback(() => { clearTimer(); setConn("error"); }, [clearTimer]);
  // Unexpected ws close: keep the red error if we were errored, else hide.
  const closed = useCallback(() => { clearTimer(); setConn((c) => (c === "error" ? c : null)); }, [clearTimer]);
  // Intentional teardown (switch away / suspend / stop): always hide.
  const reset = useCallback(() => { clearTimer(); setConn(null); }, [clearTimer]);

  useEffect(() => clearTimer, [clearTimer]); // clear the timer on unmount

  return { conn, connecting, connected, errored, closed, reset, waiting, reconnecting };
}
```

- [ ] **Step 4: Update `web/src/shell/ConnStatus.tsx`**

Add the two states to `DOT` and `LABEL`:

```tsx
const DOT: Record<ConnState, string> = {
  waiting: "bg-zinc-400 animate-pulse",
  connecting: "bg-zinc-400 animate-pulse",
  reconnecting: "bg-yellow-400 animate-pulse",
  slow: "bg-amber-500 animate-pulse",
  connected: "bg-green-500",
  error: "bg-red-500",
  disconnected: "bg-red-500",
};
const LABEL: Record<ConnState, string> = {
  waiting: "waiting",
  connecting: "connecting…",
  reconnecting: "reconnecting…",
  slow: "connecting…",
  connected: "connected",
  error: "error",
  disconnected: "disconnected",
};
```

(The rest of `ConnStatus.tsx` is unchanged.)

- [ ] **Step 5: Run tests + typecheck**

Run (from `web/`): `npx vitest run src/shell/useConnStatus.test.ts && npx tsc --noEmit`
Expected: PASS (the new + existing useConnStatus tests) and tsc clean. tsc will flag any
`Record<ConnState, …>` that's now missing a key — both `DOT` and `LABEL` must list all 7 states.

- [ ] **Step 6: Commit**

```bash
git add web/src/shell/useConnStatus.ts web/src/shell/ConnStatus.tsx web/src/shell/useConnStatus.test.ts
git commit --no-verify -m "feat(web): reconnecting/disconnected conn states + grace timer [sp-yry]"
```

---

## Task 3: Wire `ReconnectingSocket` into `App.tsx`

**Files:**
- Modify: `web/src/App.tsx`

No new unit test (the `openSession` lifecycle isn't unit-tested in this codebase — the pure
`nextConnAction` and `useConnStatus`/wrapper bits are; this task is integration wiring verified by
`tsc` + the e2e suite). The current `openSession`, `teardown`, `wsRef`, and the
`useConnStatus` destructure are the touch points.

- [ ] **Step 1: Add imports + the `reconnecting` action**

At the top of `web/src/App.tsx`, add the import:
```ts
import { ReconnectingSocket } from "./shell/reconnectingSocket";
```
Change the `useConnStatus` destructure to include `reconnecting`:
```ts
const { conn, connecting, connected, errored, closed, reset, waiting, reconnecting } = useConnStatus();
```

- [ ] **Step 2: Retype `wsRef`**

Change the ref type from `WebSocket` to the wrapper:
```ts
const wsRef = useRef<ReconnectingSocket | null>(null);
```
(`teardown`'s `wsRef.current?.close()`, the unmount effect's `wsRef.current?.close()`, and
`refreshSpawns`'s `!!wsRef.current` all keep working — `close()` now also stops reconnection.)

- [ ] **Step 3: Rewrite `openSession` to use `ReconnectingSocket`**

Replace the entire current `openSession` with:

```ts
  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    connecting();
    // partysocket fires onOpen on EVERY (re)connection: re-send the bind frame (the CP's HandleWS
    // expects it first on each new underlying socket) and re-run the ACP handshake. A fresh Client
    // per open gives a clean Conn buffer + cleared pending, so a truncated frame from a dropped
    // socket can't corrupt the new stream; the node replays spawn/history -> transcript restores.
    const sock = new ReconnectingSocket(`ws://${location.host}/ws/session`, {
      onOpen: async () => {
        if (genRef.current !== gen) return;
        sock.send(JSON.stringify({ spawnId, token: DEV_TOKEN }));
        const c = new Client(sock);
        clientRef.current = c;
        c.onHistory = (h) => {
          if (genRef.current !== gen) return;
          const its = historyToItems(h).map(withId);
          buffersRef.current.set(spawnId, its);
          if (activeIdRef.current === spawnId) setItems(its);
        };
        try {
          await c.initialize();
          await c.newSession("/app");
        } catch {
          if (genRef.current !== gen) return;
          reconnecting(); // handshake failed -> the socket keeps retrying
          return;
        }
        if (genRef.current !== gen) return;
        connected();
      },
      onDown: () => {
        if (genRef.current !== gen) return;
        reconnecting(); // dropped/failed attempt -> yellow, then red after grace; partysocket retries
      },
    });
    wsRef.current = sock;
  };
```

(The previous `ws.onerror`/`ws.onclose` handlers are gone — partysocket surfaces those via
`onDown` (close/error) and never gives up, so there's no terminal `closed()`/`errored()` for the
socket itself. The ledger poll still drives `errored()`/teardown for a dead *spawn*.)

- [ ] **Step 4: Typecheck + full web unit suite**

Run (from `web/`): `npx tsc --noEmit && npx vitest run`
Expected: tsc clean; all unit tests pass (Task 1 + Task 2 + existing). `new Client(sock)` must
typecheck — `ReconnectingSocket` implements `WebSocketLike` (the `Client` constructor's param), so
no cast is needed; if tsc complains, confirm `WebSocketLike` matches the wrapper's `binaryType`/
`onmessage`/`send` and fix the wrapper's types (do NOT widen `WebSocketLike`/`Conn`).

- [ ] **Step 5: Commit**

```bash
git add web/src/App.tsx
git commit --no-verify -m "feat(web): auto-reconnecting chat socket; handshake re-runs per (re)connect [sp-yry]"
```

- [ ] **Step 6: e2e confirmation (orchestrator-run)**

The implementer's sandbox may kill the long browser+Docker run, so the **orchestrator** runs this:

Run (from `web/`): `npx playwright test`
Expected: the existing suite passes (a test may be `flaky` and pass on retry — the known
`net::ERR_NETWORK_CHANGED` / marketplace page-load env flake — that counts as passing). The happy
path is unchanged: the first connect succeeds fast, one `onOpen` runs the handshake → header
"connected". A regression here (spawns not reaching "connected", or the transcript/reload tests
failing) means the `onOpen` rewrite or the bind-frame/handshake re-run broke the path — STOP and
investigate.

---

## Manual verification (not automated)

`just dev`, open a spawn (header → green "connected"). Kill the node (`docker stop` the node
container / restart `just dev`'s node) → header goes yellow "reconnecting…", then red
"disconnected" after ~12s, while it keeps retrying; bring the node back → it reconnects to green on
its own (no page reload, no manual spawn-switch). This is the behavior the old code couldn't do.

## Out of scope (note in the bead, not built here)

- Post-`open` ACP handshake timeout (agent silent after the socket opens) — separate follow-up;
  the sp-39u readiness probe makes it unlikely.
