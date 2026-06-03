# Spawn Starting Lifecycle (sp-wi3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Make `CreateSpawn` async so the spawn shows a visible `starting` (yellow) → `active` (green) / `error` (red) lifecycle; the web gates the WS on `active` and shows a "waiting" header state while starting.

**Architecture:** CP `CreateSpawn` validates synchronously, writes the spawn `starting`, returns the id immediately, and provisions in a background goroutine that drives `SetActive`/`SetError` on the node signal. The web reflects the ledger: a poll (1s while any spawn is starting, else 3s) connects the WS when the active spawn flips to `active`, shows red on `error`, and shows a grey-pulse "waiting" while `starting`.

**Tech Stack:** Go (CP), React/TypeScript (web), Vitest, Playwright.

**Conventions:**
- Commits `git commit --no-verify`. Local-only repo, no push. Do NOT touch `.beads/`.
- Web from `web/`; `npx tsc --noEmit` must be clean after web changes.
- Reference spec: `docs/superpowers/specs/2026-06-02-spawn-starting-lifecycle-design.md`.

---

### Task A: CP — async `CreateSpawn` + tests

**Files:**
- Modify: `internal/cp/server.go` (`CreateSpawn` → async + new `provisionSpawn`)
- Modify: `internal/cp/version_select_test.go` (`createActive` waits for active)
- Modify: `internal/cp/lifecycle_test.go` (add `waitActive`; suspend/resume tests wait; 2 new async tests)

- [ ] **Step 1: Write the failing async tests** — append to `internal/cp/lifecycle_test.go`. First add a `waitActive` helper (used by createActive too) + the two tests:

```go
// waitActive polls the store until the spawn reaches active (CreateSpawn is async now). Fails on
// timeout or if the spawn errors first.
func waitActive(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, err := s.st.Spawns().Get(ctx, id)
		if err == nil && sp.Status == store.Active {
			return
		}
		if err == nil && sp.Status == store.Errored {
			t.Fatalf("spawn %s errored while waiting for active", id)
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s not active within 3s", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCreateSpawnIsAsyncReturnsStarting(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1}) // present but we don't ack yet
	ctx := auth.WithOwner(context.Background(), "alice")

	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	// Returns immediately with the spawn still 'starting' (provisioning is in flight, no ACTIVE yet).
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
	if err != nil || sp.Status != store.Starting {
		t.Fatalf("status=%v err=%v want starting (async create)", sp.Status, err)
	}
	// Wait until the node was sent StartSpawn, then ack ACTIVE -> the background provision finishes.
	deadline := time.Now().Add(2 * time.Second)
	for sender.firstStart() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no StartSpawn was sent")
		}
		time.Sleep(time.Millisecond)
	}
	s.sched.OnStatus(resp.Msg.SpawnId, nodev1.SpawnPhase_ACTIVE)
	waitActive(t, s, resp.Msg.SpawnId)
}

func TestCreateSpawnProvisionFailureSetsError(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "n1", Sender: sender, Max: 1, Free: 1})
	go func() { // ack ERROR once StartSpawn is sent
		deadline := time.Now().Add(2 * time.Second)
		for {
			if st := sender.firstStart(); st != nil {
				s.sched.OnStatus(st.GetSpawnId(), nodev1.SpawnPhase_ERROR)
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{AppId: "secret-app", Model: "m"}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sp, _ := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
		if sp.Status == store.Errored {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn %s not errored after provision failure (status=%v)", resp.Msg.SpawnId, sp.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/cp/ -run 'TestCreateSpawnIsAsync|TestCreateSpawnProvisionFailure' -count=1`. Expected: FAIL — today's `CreateSpawn` blocks until active, so `TestCreateSpawnIsAsyncReturnsStarting` sees `active` (not `starting`) once it returns, and the no-ack case makes `CreateSpawn` block/return an error.

- [ ] **Step 3: Make `CreateSpawn` async in `internal/cp/server.go`.** READ the current `CreateSpawn`. Replace its tail — the part from `nodeID, err := s.sched.Provision(...)` through the final `return connect.NewResponse(...)` — with an async launch. The block to replace is:

```go
	nodeID, err := s.sched.Provision(ctx, spawnID, ver.Ref, req.Msg.Model, placement)
	if err != nil {
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("CreateSpawn %s: SetError after provision failure also failed: %v", spawnID, serr)
		}
		return nil, err
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		// Orphan-window compensation: the node container is live + the route is bound, but we
		// couldn't record active — tear it down so we don't leak a container/route.
		s.rt.StopOnNode(spawnID)
		s.rt.Drop(spawnID)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("CreateSpawn %s: SetError after SetActive failure also failed: %v", spawnID, serr)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
}
```

with:

```go
	// Provision asynchronously: return the spawn in 'starting' immediately; the background goroutine
	// drives it to active/error on the node's signal, so the UI can show a 'starting' period. The
	// request ctx is done once we return, so the goroutine uses a detached ctx.
	go s.provisionSpawn(context.WithoutCancel(ctx), spawnID, ver.Ref, req.Msg.Model, placement)
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
}

// provisionSpawn runs the async provision for a spawn that CreateSpawn left in 'starting'. It takes
// the per-spawn lock (serializing a Stop/Suspend during starting AFTER it) and bails if the spawn was
// already stopped in the lock gap; then Provision -> SetActive, or SetError on failure, with the same
// teardown compensation as the old inline path on a post-provision SetActive failure.
func (s *Server) provisionSpawn(ctx context.Context, spawnID, appRef, model string, placement registry.Placement) {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	if sp, err := s.st.Spawns().Get(ctx, spawnID); err != nil || sp.Status != store.Starting {
		return // stopped/deleted in the lock gap, or already advanced
	}
	nodeID, err := s.sched.Provision(ctx, spawnID, appRef, model, placement)
	if err != nil {
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after provision failure also failed: %v", spawnID, serr)
		}
		return
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		s.rt.StopOnNode(spawnID)
		s.rt.Drop(spawnID)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after SetActive failure also failed: %v", spawnID, serr)
		}
	}
}
```

Note: the synchronous prefix (auth, quota, version, mounts, `placementFor`, mint id, lock, name, `store.Create`) is UNCHANGED — validation errors still return synchronously. The `defer unlock()` in `CreateSpawn` releases the lock when it returns (right after launching the goroutine); the goroutine then acquires its own lock. `context`, `registry`, `store`, `log` are already imported in server.go.

- [ ] **Step 4: Run the new tests** — `go test ./internal/cp/ -run 'TestCreateSpawnIsAsync|TestCreateSpawnProvisionFailure' -count=1` → PASS.

- [ ] **Step 5: Update `createActive` to wait for active** — in `internal/cp/version_select_test.go`, the `createActive` helper calls `CreateSpawn` then `Get`. Insert a `waitActive` call between them. Change:
```go
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
```
to:
```go
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	waitActive(t, s, resp.Msg.SpawnId) // CreateSpawn is async; wait for the background provision
	sp, err := s.st.Spawns().Get(ctx, resp.Msg.SpawnId)
```

- [ ] **Step 6: Make the suspend/resume tests wait for active** — in `internal/cp/lifecycle_test.go`, `TestSuspendSpawn` and `TestResumeSpawn` call `s.CreateSpawn(...)` then immediately operate on the spawn. After each happy-path `CreateSpawn` that you then Suspend/Resume, insert `waitActive(t, s, id)` (using the `id := resp.Msg.SpawnId` already in those tests) before the `SuspendSpawn` call. Read the tests; add the wait right after `id := resp.Msg.SpawnId`.

- [ ] **Step 7: Run the FULL cp suite + race** — `go test ./internal/cp/ -count=1` and `go test -race ./internal/cp/... -count=1`. Expected: PASS. If any OTHER test calls `CreateSpawn` and then assumes active (grep `CreateSpawn` in `internal/cp/*_test.go`), add `waitActive` there too. Validation-error tests (unknown version, unverified non-creator) still pass synchronously (no change).

- [ ] **Step 8: Commit**
```bash
git add internal/cp/server.go internal/cp/version_select_test.go internal/cp/lifecycle_test.go
git commit --no-verify -m "feat(cp): async CreateSpawn — return starting, provision in background (sp-wi3)"
```

---

### Task B: web — `waiting` conn state, yellow starting dot, `nextConnAction` policy

**Files:**
- Modify: `web/src/shell/useConnStatus.ts` (+ `.test.ts`)
- Modify: `web/src/shell/ConnStatus.tsx` (+ `.test.tsx`)
- Modify: `web/src/shell/Sidebar.tsx` (+ `.test.tsx`)
- Create: `web/src/shell/connPolicy.ts` (+ `connPolicy.test.ts`)

- [ ] **Step 1: `useConnStatus` — add `waiting`.** In `web/src/shell/useConnStatus.ts`: change `ConnState` to include `"waiting"`, and add a `waiting()` callback. Edit the type:
```ts
export type ConnState = "waiting" | "connecting" | "slow" | "connected" | "error";
```
Add the callback (next to `connected`/`errored`):
```ts
  // The selected spawn is still starting (no socket yet) — grey-pulse "waiting" until it goes active.
  const waiting = useCallback(() => { clearTimer(); setConn("waiting"); }, [clearTimer]);
```
Add `waiting` to the returned object: `return { conn, connecting, connected, errored, closed, reset, waiting };`

- [ ] **Step 2: hook test** — append to `web/src/shell/useConnStatus.test.ts`:
```ts
  it("waiting sets the waiting state and clears any slow timer", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.waiting());
    expect(result.current.conn).toBe("waiting");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("waiting"); // no slow flip from a leftover timer
  });
```
Run `cd web && npx vitest run src/shell/useConnStatus.test.ts` → PASS.

- [ ] **Step 3: `ConnStatus` — render `waiting`.** In `web/src/shell/ConnStatus.tsx`, add `waiting` to both maps:
```ts
const DOT: Record<ConnState, string> = {
  waiting: "bg-zinc-400 animate-pulse",
  connecting: "bg-zinc-400 animate-pulse",
  slow: "bg-amber-500 animate-pulse",
  connected: "bg-green-500",
  error: "bg-red-500",
};
const LABEL: Record<ConnState, string> = {
  waiting: "waiting",
  connecting: "connecting…",
  slow: "connecting…",
  connected: "connected",
  error: "error",
};
```

- [ ] **Step 4: component test** — append a row to the `it.each` in `web/src/shell/ConnStatus.test.tsx`:
```ts
    ["waiting", "waiting", "bg-zinc-400"],
```
(add it as the first tuple in the existing `it.each([...])` array). Run `cd web && npx vitest run src/shell/ConnStatus.test.tsx` → PASS.

- [ ] **Step 5: Sidebar — yellow starting dot.** In `web/src/shell/Sidebar.tsx`, the `DOT` map currently has `starting: "bg-amber-500 animate-pulse"`. Change it to yellow:
```ts
  starting: "bg-yellow-400 animate-pulse",
```
(leave `suspending: "bg-amber-500 animate-pulse"` unchanged.)

- [ ] **Step 6: Sidebar test** — in `web/src/shell/Sidebar.test.tsx`, add a `starting` spawn to the `spawns` fixture and assert its dot status. Add to the `spawns` array a third entry:
```ts
  { spawnId: "c", name: "Starting One", appId: "spawnery/wiki", status: "starting" },
```
and add a test:
```ts
  it("renders a yellow pulsing dot for a starting spawn", () => {
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={noopActions} />);
    const dot = screen.getByTestId("spawn-dot-c");
    expect(dot.getAttribute("data-status")).toBe("starting");
    expect(dot.className).toContain("bg-yellow-400");
  });
```
(adjust the existing "lists spawns…" count assertions if any count rows — read the file; if a test asserts a row count, update it for the added spawn.) Run `cd web && npx vitest run src/shell/Sidebar.test.tsx` → PASS.

- [ ] **Step 7: `connPolicy.ts` — the poll decision (pure, testable).** Create `web/src/shell/connPolicy.ts`:
```ts
import type { SpawnStatus } from "@/api/spawnlet";
import type { ConnState } from "./useConnStatus";

export type ConnAction = "open" | "error" | "waiting" | "drop" | "none";

// nextConnAction decides what the poll should do for the ACTIVE spawn, given its ledger status, whether
// a live ws already exists, and the current header conn state. Transition-only: returns "none" when the
// desired state is already reflected, so the poll doesn't reopen the ws or re-set the same state.
//   - status undefined (vanished from the ledger)            -> "drop"  (tear down + clear)
//   - active & no ws                                         -> "open"  (connect the ws)
//   - error  & not already showing error                     -> "error" (red)
//   - starting & not already waiting                         -> "waiting" (grey pulse)
//   - anything else (suspended/suspending/unreachable/...)   -> "none"
export function nextConnAction(status: SpawnStatus | undefined, hasWs: boolean, conn: ConnState | null): ConnAction {
  if (status === undefined) return "drop";
  if (status === "active") return hasWs ? "none" : "open";
  if (status === "error") return conn === "error" ? "none" : "error";
  if (status === "starting") return conn === "waiting" ? "none" : "waiting";
  return "none";
}
```

- [ ] **Step 8: `connPolicy.test.ts`** — create `web/src/shell/connPolicy.test.ts`:
```ts
import { describe, it, expect } from "vitest";
import { nextConnAction } from "./connPolicy";

describe("nextConnAction", () => {
  it("vanished -> drop", () => {
    expect(nextConnAction(undefined, true, "connected")).toBe("drop");
  });
  it("active without a ws -> open; with a ws -> none", () => {
    expect(nextConnAction("active", false, "waiting")).toBe("open");
    expect(nextConnAction("active", true, "connected")).toBe("none");
  });
  it("error -> error once, then none", () => {
    expect(nextConnAction("error", true, "connected")).toBe("error");
    expect(nextConnAction("error", false, "error")).toBe("none");
  });
  it("starting -> waiting once, then none", () => {
    expect(nextConnAction("starting", false, null)).toBe("waiting");
    expect(nextConnAction("starting", false, "waiting")).toBe("none");
  });
  it("suspended/unknown -> none", () => {
    expect(nextConnAction("suspended", false, null)).toBe("none");
    expect(nextConnAction("unreachable", false, null)).toBe("none");
  });
});
```
Run `cd web && npx vitest run src/shell/connPolicy.test.ts` → PASS.

- [ ] **Step 9: Full web suite + type-check + commit**
```bash
cd web && npm test
cd web && npx tsc --noEmit
```
Both pass/clean. (App.tsx isn't wired to `waiting`/`nextConnAction` yet — that's Task C; these additions are non-breaking.)
```bash
git add web/src/shell/useConnStatus.ts web/src/shell/useConnStatus.test.ts web/src/shell/ConnStatus.tsx web/src/shell/ConnStatus.test.tsx web/src/shell/Sidebar.tsx web/src/shell/Sidebar.test.tsx web/src/shell/connPolicy.ts web/src/shell/connPolicy.test.ts
git commit --no-verify -m "feat(web): waiting conn state, yellow starting dot, nextConnAction policy (sp-wi3)"
```

---

### Task C: web — App lifecycle wiring (async spawn, poll-connect, input gating) + e2e

**Files:**
- Modify: `web/src/App.tsx` (full rewrite)
- Run: e2e

- [ ] **Step 1: Rewrite `web/src/App.tsx`** with the full content below (it threads the `waiting` callback + `connPolicy`, makes `spawnApp` async-create, drives the WS off the ledger poll, faster-polls while starting, and gates the chat input on `connected`):

```tsx
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Client, historyToItems } from "./acp/client";
import { AppShell } from "./shell/AppShell";
import { useConnStatus } from "./shell/useConnStatus";
import { nextConnAction } from "./shell/connPolicy";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item } from "./views/chat/types";

const MODEL = "deepseek/deepseek-v4-flash";

export function App() {
  const { conn, connecting, connected, errored, closed, reset, waiting } = useConnStatus();
  const [items, setItems] = useState<Item[]>([]);
  const [busy, setBusy] = useState(false);
  const [perm, setPerm] = useState<{ title: string; resolve: (b: boolean) => void } | null>(null);
  const [spawns, setSpawns] = useState<SpawnView[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);

  const clientRef = useRef<Client | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const idRef = useRef(0);
  const genRef = useRef(0);
  const buffersRef = useRef<Map<string, Item[]>>(new Map());
  // refs mirroring state so async callbacks (poll, ws onopen, onHistory) don't read stale closures.
  const activeIdRef = useRef<string | null>(null);
  const itemsRef = useRef<Item[]>([]);
  const spawnsRef = useRef<SpawnView[]>([]);
  const connRef = useRef(conn);

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { itemsRef.current = items; }, [items]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);
  useEffect(() => { connRef.current = conn; }, [conn]);

  // Distributive Omit so each Item variant keeps its own fields (plain Omit<union,"id"> collapses them).
  type ItemInput = Item extends infer T ? (T extends { id: number } ? Omit<T, "id"> : never) : never;
  const withId = (it: ItemInput): Item => ({ ...it, id: idRef.current++ } as Item);

  // teardown closes the live ws but leaves the header state to the caller (used for the error case,
  // which must show red, not the null that closeSession's reset() would set).
  const teardown = () => {
    genRef.current++;
    wsRef.current?.close();
    wsRef.current = null;
    clientRef.current = null;
  };
  const closeSession = () => { teardown(); reset(); };

  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    connecting();
    const ws = new WebSocket(`ws://${location.host}/ws/session`);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    ws.onopen = async () => {
      ws.send(JSON.stringify({ spawnId, token: DEV_TOKEN }));
      const c = new Client(ws as any);
      clientRef.current = c;
      // spawn/history replay: the node relay (Docker lane) or the in-pod adapter (CRI lane) sends it on
      // (re)connect, so the transcript is restored even after a browser reload wipes the in-memory buffer.
      c.onHistory = (h) => {
        if (genRef.current !== gen) return;
        const its = historyToItems(h).map(withId);
        buffersRef.current.set(spawnId, its);
        if (activeIdRef.current === spawnId) setItems(its);
      };
      try {
        await c.initialize();
        await c.newSession("/app");
      } catch (e: any) {
        if (genRef.current !== gen) return;
        errored();
        return;
      }
      if (genRef.current !== gen) return;
      connected();
    };
    ws.onerror = () => { if (genRef.current !== gen) return; errored(); toast.error("Connection error"); };
    ws.onclose = () => { if (genRef.current !== gen) return; closed(); };
  };

  // refreshSpawns fetches the ledger, reconciles the active spawn's header/WS off its status (via the
  // pure nextConnAction policy), and returns the fetched list (so the poll can pick its cadence).
  const refreshSpawns = async (): Promise<SpawnView[]> => {
    let list: SpawnView[];
    try { list = await listSpawns(); }
    catch { return spawnsRef.current; }
    setSpawns(list);
    const aid = activeIdRef.current;
    if (aid) {
      const active = list.find((s) => s.spawnId === aid);
      switch (nextConnAction(active?.status, !!wsRef.current, connRef.current)) {
        case "drop":
          closeSession();
          setActiveId(null); activeIdRef.current = null; setItems([]);
          break;
        case "open":
          openSession(aid); // just became active -> connect (green)
          break;
        case "error":
          teardown(); errored(); // failed to start -> red, stays in the sidebar
          break;
        case "waiting":
          waiting(); // still starting -> grey pulse
          break;
        case "none":
          break;
      }
    }
    return list;
  };

  // Poll the ledger: 1s while any spawn is 'starting' (snappy green/connect), else 3s.
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>;
    let stopped = false;
    const tick = async () => {
      const list = await refreshSpawns();
      if (stopped) return;
      const fast = list.some((s) => s.status === "starting");
      timer = setTimeout(tick, fast ? 1000 : 3000);
    };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
  }, []);

  // On unmount just close the live ws — spawns persist on the node.
  useEffect(() => () => { wsRef.current?.close(); }, []);

  const spawnApp = async (appId: string) => {
    try {
      const id = await createSpawn(appId, MODEL); // async CP: returns immediately, status 'starting'
      const prevId = activeIdRef.current;
      teardown(); // close any current live session before switching to the new (starting) spawn
      buffersRef.current.set(id, []);
      setActiveId(id);
      activeIdRef.current = id;
      setItems((current) => {
        if (prevId && prevId !== id) buffersRef.current.set(prevId, current);
        return [];
      });
      waiting(); // grey-pulse until the node signals active; the poll then opens the ws
      await refreshSpawns(); // sidebar shows the new spawn yellow immediately
    } catch (e: any) {
      errored();
      toast.error("Spawn failed: " + e.message);
    }
  };

  const selectSpawn = (id: string) => {
    if (id === activeIdRef.current) return;
    const prevId = activeIdRef.current;
    closeSession();
    setActiveId(id);
    activeIdRef.current = id;
    const buf = buffersRef.current.get(id) ?? [];
    setItems((current) => {
      if (prevId) buffersRef.current.set(prevId, current);
      return buf;
    });
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    if (sp?.status === "active") openSession(id);
    else if (sp?.status === "starting") waiting();
    else if (sp?.status === "error") errored();
    // suspended / unknown -> hidden (closeSession already reset())
  };

  const onRename = async (id: string, name: string) => {
    setSpawns((xs) => xs.map((s) => (s.spawnId === id ? { ...s, name } : s))); // optimistic
    try { await renameSpawn(id, name); } catch (e: any) { toast.error("Rename failed: " + e.message); }
    refreshSpawns();
  };
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      if (activeIdRef.current === id) { closeSession(); }
    } catch (e: any) { toast.error("Suspend failed: " + e.message); }
    refreshSpawns();
  };
  const onResume = async (id: string) => {
    try {
      await resumeSpawn(id);
      if (activeIdRef.current === id) openSession(id);
    } catch (e: any) { toast.error("Resume failed: " + e.message); }
    refreshSpawns();
  };
  const onStop = async (id: string) => {
    try { await deleteSpawn(id); } catch (e: any) { toast.error("Stop failed: " + e.message); }
    buffersRef.current.delete(id);
    if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); }
    refreshSpawns();
  };

  const add = (it: ItemInput) => setItems((xs) => [...xs, withId(it)]);
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: (last as { text: string }).text + t }];
      return [...xs, withId({ kind, text: t })];
    });

  const onSend = async (text: string) => {
    if (!clientRef.current) return;
    add({ kind: "user", text });
    setBusy(true);
    try {
      await clientRef.current.prompt(text, {
        onText: appendChunk("agent"),
        onThought: appendChunk("thought"),
        onToolCall: (tc) => add({ kind: "tool", title: tc.title, status: tc.status }),
        onToolUpdate: (tc) => add({ kind: "tool", title: "tool", status: tc.status }),
        requestPermission: (req) =>
          new Promise<boolean>((resolve) =>
            setPerm({ title: req?.options?.[0]?.name ?? "an action", resolve: (b) => { setPerm(null); resolve(b); } })),
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <AppShell
      conn={conn}
      items={items}
      busy={busy || conn !== "connected"}
      onSend={onSend}
      perm={perm}
      onSpawnApp={spawnApp}
      spawns={spawns}
      activeId={activeId}
      actions={{ onSelectSpawn: selectSpawn, onRename, onSuspend, onResume, onStop }}
    />
  );
}
```

Key changes vs the prior App.tsx: `spawnApp` no longer calls `openSession` (it sets `waiting()` and lets the poll connect on active); `openSession` no longer touches `busy` (the indicator carries connection state); `refreshSpawns` returns the list and drives the active spawn via `nextConnAction`; the poll is a self-rescheduling `setTimeout` (1s while starting, else 3s); the chat input is disabled via `busy={busy || conn !== "connected"}`.

- [ ] **Step 2: Full web suite + type-check**
```bash
cd web && npm test
cd web && npx tsc --noEmit
```
Both pass/clean. The existing `AppShell.test` is unaffected (it passes `conn`/`busy` props directly). If `tsc` flags anything, fix it.

- [ ] **Step 3: Run the e2e** (web-only change — vite serves it; cp/spawnlet rebuilt by global-setup; no image rebuild):
```bash
cd /home/debian/AleCode/spawnery
pkill -f 'bin/cp' 2>/dev/null; pkill -f 'bin/spawnlet' 2>/dev/null
docker rm -f $(docker ps -aq --filter ancestor=spawnery/stubagent:dev --filter ancestor=spawnery/sidecar:dev) 2>/dev/null
rm -f cp.db; rm -rf .spawns
cd web && npm run test:e2e
```
Expected: ALL e2e specs pass (10). They wait for the header "connected", which now flows waiting → connecting → connected (the async create + 1s starting-poll connect). The "two instances active dots" test polls for `data-status="active"` (the dots go yellow → green). Generous timeouts absorb the async create + provision. Report the full breakdown. If a spec times out waiting for "connected", debug (did the poll connect on active? is the spawn reaching active in the ledger?) and report — do NOT loosen assertions to force a pass.

- [ ] **Step 4: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/src/App.tsx
git commit --no-verify -m "feat(web): async spawn lifecycle — waiting -> poll-connect on active, input gated on connected (sp-wi3)"
```

---

## Definition of Done

- `CreateSpawn` returns immediately in `starting`; the background goroutine drives `active`/`error`; validation still sync. CP suite + `-race` green (createActive/suspend/resume wait-for-active; 2 new async tests).
- Web: yellow pulsing `starting` dot; selecting a starting spawn shows the header grey-pulse "waiting"; the WS connects only when the ledger flips to `active` (≤1s via the faster poll); a failed spawn stays red and removable; the chat input is disabled until connected. `useConnStatus`/`ConnStatus`/`connPolicy` unit-tested; web vitest + tsc clean; e2e 10/10.

## Out of scope
- Cancelling an in-flight starting provision (Stop waits, then stops).
- Auto-reconnect on an unexpected ws drop.
