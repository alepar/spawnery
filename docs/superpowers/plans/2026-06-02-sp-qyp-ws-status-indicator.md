# WS Status Indicator (sp-qyp) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Replace the chat-header free-text status with a WebSocket connection-status indicator — a colored, pulsing status light + label (gray-pulse "connecting…", amber-pulse after ~5s, green "connected", red "error"), hidden when there is no live socket.

**Architecture:** A `useConnStatus` hook owns the state machine + 5s "slow" timer (testable without mocking WebSocket). A `ConnStatus` presentational component renders the dot + label (or nothing when `null`). `App.tsx` swaps its free-text `status` state for the hook and calls the hook callbacks at the ws lifecycle points; `AppShell` takes a `conn` prop and renders `<ConnStatus>`.

**Tech Stack:** React + TypeScript, Vitest (`@testing-library/react`), Playwright e2e.

**Conventions:**
- Commits `git commit --no-verify`. Local-only repo, no push. Do NOT touch `.beads/`.
- Web commands from `web/`. After web changes, `npx tsc --noEmit` MUST be clean (Vitest doesn't type-check).
- Reference spec: `docs/superpowers/specs/2026-06-02-ws-status-indicator-design.md`.

---

### Task 1: `useConnStatus` hook + `ConnStatus` component (+ vitest)

**Files:**
- Create: `web/src/shell/useConnStatus.ts`
- Create: `web/src/shell/ConnStatus.tsx`
- Test: `web/src/shell/useConnStatus.test.ts`
- Test: `web/src/shell/ConnStatus.test.tsx`

- [ ] **Step 1: Write the failing hook test** — create `web/src/shell/useConnStatus.test.ts`:

```ts
import { renderHook, act } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { useConnStatus } from "./useConnStatus";

describe("useConnStatus", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("starts null (indicator hidden)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    expect(result.current.conn).toBe(null);
  });

  it("connecting -> slow after the timeout", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    expect(result.current.conn).toBe("connecting");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("slow");
  });

  it("connected before the timeout stays connected (timer cleared)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.connected());
    expect(result.current.conn).toBe("connected");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("connected");
  });

  it("errored sets error; closed keeps error; reset clears it", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.errored());
    expect(result.current.conn).toBe("error");
    act(() => result.current.closed());
    expect(result.current.conn).toBe("error"); // error preserved across an unexpected close
    act(() => result.current.reset());
    expect(result.current.conn).toBe(null);
  });

  it("closed while connecting hides (null)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.closed());
    expect(result.current.conn).toBe(null);
  });
});
```

Note: `renderHook` is exported by `@testing-library/react` (v13.1+). If the import errors, verify the installed version — it is the same package the other shell tests import `render`/`screen` from.

- [ ] **Step 2: Run to verify it fails** — `cd web && npx vitest run src/shell/useConnStatus.test.ts` → FAIL (`useConnStatus` undefined).

- [ ] **Step 3: Implement the hook** — create `web/src/shell/useConnStatus.ts`:

```ts
import { useCallback, useEffect, useRef, useState } from "react";

export type ConnState = "connecting" | "slow" | "connected" | "error";

// useConnStatus is the WS connection-status state machine for the chat-header indicator. `conn` is
// null when there is no live socket (the indicator is hidden). connecting() arms a `slowMs` timer
// that flips connecting -> slow if the socket is still connecting when it fires.
export function useConnStatus(slowMs = 5000) {
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

  const connected = useCallback(() => { clearTimer(); setConn("connected"); }, [clearTimer]);
  const errored = useCallback(() => { clearTimer(); setConn("error"); }, [clearTimer]);
  // Unexpected ws close: keep the red error if we were errored, else hide.
  const closed = useCallback(() => { clearTimer(); setConn((c) => (c === "error" ? c : null)); }, [clearTimer]);
  // Intentional teardown (switch away / suspend / stop): always hide.
  const reset = useCallback(() => { clearTimer(); setConn(null); }, [clearTimer]);

  useEffect(() => clearTimer, [clearTimer]); // clear the timer on unmount

  return { conn, connecting, connected, errored, closed, reset };
}
```

- [ ] **Step 4: Run the hook test** — `cd web && npx vitest run src/shell/useConnStatus.test.ts` → PASS.

- [ ] **Step 5: Write the failing component test** — create `web/src/shell/ConnStatus.test.tsx`:

```tsx
import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { ConnStatus } from "./ConnStatus";

describe("ConnStatus", () => {
  it("renders nothing when conn is null", () => {
    render(<ConnStatus conn={null} />);
    expect(screen.queryByTestId("status")).toBeNull();
  });

  it.each([
    ["connecting", "connecting…", "bg-zinc-400"],
    ["slow", "connecting…", "bg-amber-500"],
    ["connected", "connected", "bg-green-500"],
    ["error", "error", "bg-red-500"],
  ] as const)("renders %s with its label + dot color", (state, label, dotClass) => {
    render(<ConnStatus conn={state} />);
    const el = screen.getByTestId("status");
    expect(el.getAttribute("data-status")).toBe(state);
    expect(el.textContent).toContain(label);
    expect(el.querySelector("span")?.className).toContain(dotClass);
  });
});
```

- [ ] **Step 6: Run to verify it fails** — `cd web && npx vitest run src/shell/ConnStatus.test.tsx` → FAIL (`ConnStatus` undefined).

- [ ] **Step 7: Implement the component** — create `web/src/shell/ConnStatus.tsx`:

```tsx
import type { ConnState } from "./useConnStatus";

// Both transitional states pulse; terminal states are static.
const DOT: Record<ConnState, string> = {
  connecting: "bg-zinc-400 animate-pulse",
  slow: "bg-amber-500 animate-pulse",
  connected: "bg-green-500",
  error: "bg-red-500",
};
const LABEL: Record<ConnState, string> = {
  connecting: "connecting…",
  slow: "connecting…",
  connected: "connected",
  error: "error",
};

// ConnStatus renders the chat-header WS connection light + label. It renders nothing when there is
// no live socket (conn === null).
export function ConnStatus({ conn }: { conn: ConnState | null }) {
  if (!conn) return null;
  return (
    <span data-testid="status" data-status={conn} className="flex items-center gap-1.5 text-xs text-muted-foreground">
      <span className={`inline-block h-2 w-2 rounded-full ${DOT[conn]}`} />
      {LABEL[conn]}
    </span>
  );
}
```

- [ ] **Step 8: Run the component test + type-check** — `cd web && npx vitest run src/shell/ConnStatus.test.tsx && npx tsc --noEmit` → PASS / clean.

- [ ] **Step 9: Commit**
```bash
git add web/src/shell/useConnStatus.ts web/src/shell/useConnStatus.test.ts web/src/shell/ConnStatus.tsx web/src/shell/ConnStatus.test.tsx
git commit --no-verify -m "feat(web): useConnStatus hook + ConnStatus indicator component (sp-qyp)"
```

---

### Task 2: Wire into App + AppShell; update tests + e2e selectors; run e2e

**Files:**
- Modify: `web/src/App.tsx`
- Modify: `web/src/shell/AppShell.tsx`
- Modify: `web/src/shell/AppShell.test.tsx`
- Modify: `web/e2e/chat.spec.ts`, `web/e2e/spawn-lifecycle.spec.ts`, `web/e2e/marketplace.spec.ts`

- [ ] **Step 1: Rewire `web/src/App.tsx`** — READ the file, then apply these EXACT edits (each `setStatus(...)` call is replaced; after these there must be NO `status`/`setStatus` left):

1. Add the hook import after the `AppShell` import (line 8 `import { AppShell } from "./shell/AppShell";`):
```ts
import { useConnStatus } from "./shell/useConnStatus";
```
2. Replace the status state declaration:
```ts
  const [status, setStatus] = useState("");
```
with:
```ts
  const { conn, connecting, connected, errored, closed, reset } = useConnStatus();
```
3. In `refreshSpawns`, replace `setActiveId(null); setItems([]); setStatus("");` with `setActiveId(null); setItems([]); reset();`.
4. In `closeSession`, add `reset();` as the last line (after `clientRef.current = null;`):
```ts
  const closeSession = () => {
    genRef.current++;
    wsRef.current?.close();
    wsRef.current = null;
    clientRef.current = null;
    reset();
  };
```
5. In `openSession`, replace `setBusy(true); setStatus("starting…");` with `setBusy(true); connecting();`.
6. In `openSession`'s `catch`, replace `setStatus("error: " + e.message); setBusy(false);` with `errored(); setBusy(false);`.
7. In `openSession`'s success tail, replace `setStatus("ready"); setBusy(false);` with `connected(); setBusy(false);`.
8. In `openSession`'s `ws.onerror`, replace `setStatus("connection error"); toast.error("Connection error");` with `errored(); toast.error("Connection error");`.
9. In `openSession`'s `ws.onclose`, replace `setStatus("session ended");` with `closed();`.
10. In `spawnApp`, replace `setBusy(true); setStatus("starting…");` with `setBusy(true); connecting();`.
11. In `spawnApp`'s `catch`, replace `setStatus("error: " + e.message); setBusy(false);` with `errored(); setBusy(false);`.
12. In `selectSpawn`, replace the two-line tail
```ts
    if (sp?.status === "active") openSession(id);
    else setStatus(sp?.status ?? "");
```
with (drop the `else` — `closeSession()` above already `reset()` to null, so a non-active spawn shows nothing):
```ts
    if (sp?.status === "active") openSession(id);
```
13. In `onSuspend`, replace `if (activeIdRef.current === id) { closeSession(); setStatus("suspended"); }` with `if (activeIdRef.current === id) { closeSession(); }`.
14. In `onStop`, replace `if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setStatus(""); }` with `if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); }`.
15. In the returned `<AppShell ... />`, replace the prop `status={status}` with `conn={conn}`.

- [ ] **Step 2: Rewire `web/src/shell/AppShell.tsx`** — READ it, then:

1. Add imports (after the existing `Sidebar` import line):
```ts
import { ConnStatus } from "./ConnStatus";
import type { ConnState } from "./useConnStatus";
```
2. In the props destructure + type, replace `status` with `conn`. Change `status, items, busy, ...` to `conn, items, busy, ...` and the type line `status: string;` to `conn: ConnState | null;`.
3. Replace the header status span. Find:
```tsx
          <span data-testid="status" className="text-xs text-muted-foreground">{status}</span>
```
and replace with:
```tsx
          <ConnStatus conn={conn} />
```

- [ ] **Step 3: Update `web/src/shell/AppShell.test.tsx`** — READ it. In `baseProps`, replace `status: "ready",` with `conn: "connected" as const,`. (The tests don't assert the status text, so no other change is needed; this just satisfies the new prop type.)

- [ ] **Step 4: Run the full web unit suite + type-check**
```bash
cd web && npm test
cd web && npx tsc --noEmit
```
Expected: all unit tests pass (incl. the new hook/component tests + the updated AppShell test); `tsc` clean. If `tsc` flags a leftover `status` reference in App.tsx/AppShell.tsx, fix it (every `status`/`setStatus` must be gone).

- [ ] **Step 5: Update the e2e selectors** that assert the header text `"ready"` → `"connected"` (the indicator's connected label). In each file, find every:
```ts
await expect(page.getByTestId("status")).toHaveText("ready", { timeout: <N> });
```
and replace with:
```ts
await expect(page.getByTestId("status")).toContainText("connected", { timeout: <N> });
```
(keep the same timeout). Apply in:
- `web/e2e/chat.spec.ts` — the `spawnSecretApp` helper AND the theme-toggle test's re-select assertion (two occurrences).
- `web/e2e/spawn-lifecycle.spec.ts` — the `spawnFromMarket` helper (one occurrence).
- `web/e2e/marketplace.spec.ts` — the spawn-flow test (one occurrence).
Read each file to find the exact occurrences; there are no other `getByTestId("status")` assertions to change. (`toContainText` because the indicator span now contains a dot child; "connecting…" does not contain "connected", so the auto-wait correctly waits for the connected state.)

- [ ] **Step 6: Run the e2e** (web-only change — vite serves the new web; global-setup builds cp/spawnlet; no image rebuild needed):
```bash
cd /home/debian/AleCode/spawnery
pkill -f 'bin/cp' 2>/dev/null; pkill -f 'bin/spawnlet' 2>/dev/null
docker rm -f $(docker ps -aq --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
rm -f cp.db; rm -rf .spawns
cd web && npm run test:e2e
```
Expected: ALL e2e specs pass (10 total). The header now shows the green "connected" indicator once a spawn is ready; the specs wait for "connected". Report the full breakdown.

- [ ] **Step 7: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/src/App.tsx web/src/shell/AppShell.tsx web/src/shell/AppShell.test.tsx web/e2e/chat.spec.ts web/e2e/spawn-lifecycle.spec.ts web/e2e/marketplace.spec.ts
git commit --no-verify -m "feat(web): WS status indicator in chat header (sp-qyp)"
```

---

## Definition of Done

- The chat header shows a colored, pulsing status light + label driven by the live ws state
  (gray-pulse connecting… / amber-pulse after ~5s / green connected / red error), hidden when no
  live socket.
- `useConnStatus` + `ConnStatus` are unit-tested (incl. the fake-timers slow transition and
  error-preserved-on-close); `AppShell.test` updated; full web vitest + `tsc --noEmit` clean.
- App.tsx has no remaining `status`/`setStatus` free-text; the e2e specs assert "connected" and pass
  (10/10) with real containers.

## Out of scope
- A per-spawn connection light in the sidebar (the sidebar already has a ledger-status dot).
- Auto-reconnect/retry UI.
