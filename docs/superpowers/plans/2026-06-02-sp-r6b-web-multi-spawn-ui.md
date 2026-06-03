# Web Multi-Spawn UI (Slice 3, sp-r6b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`).

**Goal:** Turn the web UI into a multi-spawn surface: the sidebar lists every running spawn (independent name headline + app-id subline + status dot + kebab actions), spawns can be renamed/suspended/resumed/stopped, multiple instances of the same app coexist, switching between spawns restores each one's transcript, and the active spawn's transcript is repopulated by the adapter's `spawn/history` replay (CRI lane) or the client-side buffer (all lanes).

**Architecture:** `App.tsx` holds a `spawns: SpawnView[]` list (hydrated from `ListSpawns`, polled every 3 s), an `activeId`, and a **single live ws/Client for the active spawn**. Per-spawn transcripts are cached in a `buffersRef` (a `Map<spawnId, Item[]>`) so switching restores them in-session (works in every lane); the adapter's `onHistory` callback (slice 2) overwrites the active buffer with the authoritative transcript when present (CRI lane). The sidebar is presentational (`SpawnView[]` + a `SpawnActions` callback bag). New CP RPCs (`ListSpawns`/`RenameSpawn`/`SuspendSpawn`/`ResumeSpawn`/`DeleteSpawn`, all from slices 1) are wrapped in `api/spawnlet.ts`.

**Tech Stack:** React + Vite, TypeScript, Vitest (unit), Playwright (e2e), Tailwind/shadcn.

**Conventions:**
- Commits `git commit --no-verify`. Local-only repo, no push. Do NOT touch `.beads/`.
- Web commands run from `web/`. After every web task, `npx tsc --noEmit` MUST pass (Vitest/esbuild does NOT type-check — a green `npm test` is not enough).
- Reference spec: `docs/superpowers/specs/2026-06-02-spawn-lifecycle-ui-design.md` §Slice 3.

**Lane note (important, not a bug):** the recording adapter (slice 2) only runs in the CRI/runsc lane (`ACP_ADAPTER=1`). In the Docker/self-hosted lane used by `just dev` and the e2e, goose/stub is attached directly (no adapter) so `onHistory` never fires there — switch-back history comes from the client-side `buffersRef`. Both paths are wired; the e2e validates the buffer path.

**Key facts:**
- `api/connect.ts` `unary<T>(method, body)` POSTs Connect-JSON (camelCase, enums as STRING names like `"SPAWN_STATUS_ACTIVE"`).
- shadcn components present: badge, button, card, collapsible, dialog, input, sonner, switch, textarea (NO dropdown-menu — the kebab menu is hand-rolled).
- `Client` (slice 2) has a settable `onHistory?: (items: HistoryItem[]) => void` and exported `historyToItems(items): ItemInput[]` (where `ItemInput = Omit<Item,"id">` distributed over the union).
- chat `Item` (`web/src/views/chat/types.ts`): `{kind:"user"|"agent"|"thought", text}` | `{kind:"tool", title, status?}` (+ `id`).
- The chat header `data-testid="status"` shows the ACP CONNECTION state ("ready"/"starting…"); the sidebar dot shows the LEDGER status — keep these distinct.

---

### Task 1: API layer — spawn lifecycle methods + `SpawnView`

**Files:**
- Modify: `web/src/api/spawnlet.ts`
- Test: `web/src/api/spawnlet.test.ts` (new)

- [ ] **Step 1: Write the failing test** — create `web/src/api/spawnlet.test.ts`:

```typescript
import { describe, it, expect, vi, afterEach } from "vitest";
import { listSpawns, renameSpawn, statusFromProto } from "./spawnlet";

function mockFetch(json: unknown) {
  const calls: { url: string; body: any }[] = [];
  const f = vi.fn(async (url: string, init: any) => {
    calls.push({ url, body: JSON.parse(init.body) });
    return { ok: true, json: async () => json, text: async () => "" } as any;
  });
  (globalThis as any).fetch = f;
  return calls;
}
afterEach(() => { vi.restoreAllMocks(); });

describe("statusFromProto", () => {
  it("maps proto enum names to short statuses", () => {
    expect(statusFromProto("SPAWN_STATUS_ACTIVE")).toBe("active");
    expect(statusFromProto("SPAWN_STATUS_SUSPENDED")).toBe("suspended");
    expect(statusFromProto("SPAWN_STATUS_ERROR")).toBe("error");
    expect(statusFromProto("SPAWN_STATUS_UNREACHABLE")).toBe("unreachable");
    expect(statusFromProto(undefined)).toBe("unknown");
    expect(statusFromProto("SPAWN_STATUS_BOGUS")).toBe("unknown");
  });
});

describe("listSpawns", () => {
  it("POSTs ListSpawns and maps the response to SpawnView[]", async () => {
    const calls = mockFetch({
      spawns: [
        { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "SPAWN_STATUS_ACTIVE" },
        { spawnId: "b", name: "", appId: "spawnery/zork", status: "SPAWN_STATUS_SUSPENDED" },
      ],
    });
    const out = await listSpawns();
    expect(calls[0].url).toContain("/cp.v1.SpawnService/ListSpawns");
    expect(out).toEqual([
      { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active" },
      { spawnId: "b", name: "", appId: "spawnery/zork", status: "suspended" },
    ]);
  });
  it("tolerates a missing spawns array", async () => {
    mockFetch({});
    expect(await listSpawns()).toEqual([]);
  });
});

describe("renameSpawn", () => {
  it("POSTs RenameSpawn with spawnId + name", async () => {
    const calls = mockFetch({});
    await renameSpawn("a", "New Name");
    expect(calls[0].url).toContain("/cp.v1.SpawnService/RenameSpawn");
    expect(calls[0].body).toEqual({ spawnId: "a", name: "New Name" });
  });
});
```

- [ ] **Step 2: Run to verify it fails** — `cd web && npx vitest run src/api/spawnlet.test.ts` → FAIL (`listSpawns`/`renameSpawn`/`statusFromProto` undefined).

- [ ] **Step 3: Implement** — replace the contents of `web/src/api/spawnlet.ts` with:

```typescript
import { unary } from "./connect";
export { DEV_TOKEN } from "./connect";

export type SpawnStatus =
  | "starting" | "active" | "suspending" | "suspended" | "unreachable" | "error" | "unknown";

export interface SpawnView {
  spawnId: string;
  name: string;
  appId: string;
  status: SpawnStatus;
}

// statusFromProto maps the Connect-JSON enum NAME (e.g. "SPAWN_STATUS_ACTIVE") to a short status.
export function statusFromProto(s: string | undefined): SpawnStatus {
  switch (s) {
    case "SPAWN_STATUS_STARTING": return "starting";
    case "SPAWN_STATUS_ACTIVE": return "active";
    case "SPAWN_STATUS_SUSPENDING": return "suspending";
    case "SPAWN_STATUS_SUSPENDED": return "suspended";
    case "SPAWN_STATUS_UNREACHABLE": return "unreachable";
    case "SPAWN_STATUS_ERROR": return "error";
    default: return "unknown";
  }
}

export async function createSpawn(appId: string, model: string): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appId, model });
  return r.spawnId;
}

export async function listSpawns(): Promise<SpawnView[]> {
  const r = await unary<{ spawns?: Array<{ spawnId: string; name?: string; appId?: string; status?: string }> }>(
    "ListSpawns", {},
  );
  return (r.spawns ?? []).map((s) => ({
    spawnId: s.spawnId,
    name: s.name ?? "",
    appId: s.appId ?? "",
    status: statusFromProto(s.status),
  }));
}

export async function renameSpawn(spawnId: string, name: string): Promise<void> {
  await unary<Record<string, never>>("RenameSpawn", { spawnId, name });
}
export async function suspendSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("SuspendSpawn", { spawnId });
}
export async function resumeSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("ResumeSpawn", { spawnId });
}
// UI "Stop" = DeleteSpawn (soft-delete; drops from the list). Legacy stopSpawn kept for non-UI callers.
export async function deleteSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("DeleteSpawn", { spawnId });
}
export async function stopSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("StopSpawn", { spawnId });
}
```

- [ ] **Step 4: Run the test + type-check** — `cd web && npx vitest run src/api/spawnlet.test.ts && npx tsc --noEmit` → both PASS/clean.

- [ ] **Step 5: Commit**
```bash
git add web/src/api/spawnlet.ts web/src/api/spawnlet.test.ts
git commit --no-verify -m "feat(web): spawn lifecycle api (list/rename/suspend/resume/delete) + SpawnView (sp-r6b)"
```

---

### Task 2: Sidebar — multi-row list, status dots, kebab actions, inline rename

**Files:**
- Modify: `web/src/shell/Sidebar.tsx` (full rewrite)
- Test: `web/src/shell/Sidebar.test.tsx` (full rewrite)

- [ ] **Step 1: Write the failing test** — replace `web/src/shell/Sidebar.test.tsx` with:

```typescript
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { Sidebar } from "./Sidebar";
import type { SpawnView } from "@/api/spawnlet";

const spawns: SpawnView[] = [
  { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active" },
  { spawnId: "b", name: "Zork 2", appId: "spawnery/zork", status: "suspended" },
];
const noopActions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onStop: vi.fn() };

describe("Sidebar", () => {
  it("renders nav (market+settings, no chat tab)", () => {
    render(<Sidebar view="market" onSelect={vi.fn()} />);
    expect(screen.getByTestId("nav-market")).toBeTruthy();
    expect(screen.getByTestId("nav-settings")).toBeTruthy();
    expect(screen.queryByTestId("nav-chat")).toBeNull();
  });

  it("shows the empty placeholder with no spawns", () => {
    render(<Sidebar view="market" onSelect={vi.fn()} spawns={[]} />);
    expect(screen.getByText("— none yet —")).toBeTruthy();
  });

  it("lists spawns with name headline, app-id subline, and a status dot", () => {
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={noopActions} />);
    expect(screen.getByTestId("spawn-name-a").textContent).toContain("Wiki");
    expect(screen.getByTestId("spawn-row-a").textContent).toContain("spawnery/wiki");
    expect(screen.getByTestId("spawn-dot-a").getAttribute("data-status")).toBe("active");
    expect(screen.getByTestId("spawn-dot-b").getAttribute("data-status")).toBe("suspended");
  });

  it("selects a spawn on row click", async () => {
    const actions = { ...noopActions, onSelectSpawn: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-select-b"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("b");
  });

  it("kebab → Suspend for an active spawn, Resume for a suspended spawn", async () => {
    const actions = { ...noopActions, onSuspend: vi.fn(), onResume: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-suspend-a"));
    expect(actions.onSuspend).toHaveBeenCalledWith("a");
    await userEvent.click(screen.getByTestId("spawn-kebab-b"));
    await userEvent.click(screen.getByTestId("spawn-resume-b"));
    expect(actions.onResume).toHaveBeenCalledWith("b");
  });

  it("kebab → Stop asks for confirm, then calls onStop", async () => {
    const actions = { ...noopActions, onStop: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-stop-a"));
    expect(actions.onStop).not.toHaveBeenCalled(); // first click = arm confirm
    await userEvent.click(screen.getByTestId("spawn-stop-confirm-a"));
    expect(actions.onStop).toHaveBeenCalledWith("a");
  });

  it("double-click name → inline edit → Enter renames", async () => {
    const actions = { ...noopActions, onRename: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.dblClick(screen.getByTestId("spawn-name-a"));
    const input = screen.getByTestId("spawn-name-input-a");
    await userEvent.clear(input);
    await userEvent.type(input, "Renamed{Enter}");
    expect(actions.onRename).toHaveBeenCalledWith("a", "Renamed");
  });
});
```

- [ ] **Step 2: Run to verify it fails** — `cd web && npx vitest run src/shell/Sidebar.test.tsx` → FAIL.

- [ ] **Step 3: Implement** — replace `web/src/shell/Sidebar.tsx` with:

```tsx
import { useState } from "react";
import { Button } from "@/components/ui/button";
import type { SpawnView, SpawnStatus } from "@/api/spawnlet";

export type View = "chat" | "market" | "settings";

// SpawnActions is the callback bag App passes down for spawn lifecycle controls.
export interface SpawnActions {
  onSelectSpawn: (spawnId: string) => void;
  onRename: (spawnId: string, name: string) => void;
  onSuspend: (spawnId: string) => void;
  onResume: (spawnId: string) => void;
  onStop: (spawnId: string) => void;
}

const NAV: { id: View; label: string }[] = [
  { id: "market", label: "Marketplace" },
  { id: "settings", label: "Settings" },
];

// dot color per ledger status: green=online, grey=suspended, red=failed, amber(pulse)=transitional.
const DOT: Record<SpawnStatus, string> = {
  active: "bg-green-500",
  suspended: "bg-zinc-400",
  suspending: "bg-amber-500 animate-pulse",
  starting: "bg-amber-500 animate-pulse",
  unreachable: "bg-red-500",
  error: "bg-red-500",
  unknown: "bg-zinc-400",
};

export function Sidebar({ view, onSelect, spawns = [], activeId, actions }: {
  view: View;
  onSelect: (v: View) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
}) {
  return (
    <nav data-testid="sidebar" className="flex w-52 flex-col gap-1 border-r border-border bg-card p-3">
      <div className="px-2 pb-3 text-sm font-semibold">Spawnery</div>
      {NAV.map((n) => (
        <Button
          key={n.id}
          data-testid={`nav-${n.id}`}
          variant={view === n.id ? "secondary" : "ghost"}
          className="justify-start"
          onClick={() => onSelect(n.id)}
        >
          {n.label}
        </Button>
      ))}
      <div className="mt-4 px-2 text-xs text-muted-foreground">Spawns</div>
      {spawns.length === 0 ? (
        <div className="px-2 text-xs text-muted-foreground/70">— none yet —</div>
      ) : (
        spawns.map((s) => (
          <SpawnRow key={s.spawnId} spawn={s} active={view === "chat" && s.spawnId === activeId} actions={actions} />
        ))
      )}
    </nav>
  );
}

function SpawnRow({ spawn, active, actions }: { spawn: SpawnView; active: boolean; actions?: SpawnActions }) {
  const [menu, setMenu] = useState(false);
  const [confirmStop, setConfirmStop] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(spawn.name);

  const startEdit = () => { setMenu(false); setDraft(spawn.name); setEditing(true); };
  const commit = () => {
    setEditing(false);
    const n = draft.trim();
    if (n && n !== spawn.name) actions?.onRename(spawn.spawnId, n);
  };

  return (
    <div
      data-testid={`spawn-row-${spawn.spawnId}`}
      className={`relative flex items-start rounded-md ${active ? "bg-secondary" : "hover:bg-accent"}`}
    >
      <button
        data-testid={`spawn-select-${spawn.spawnId}`}
        className="flex min-w-0 flex-1 flex-col items-start gap-0.5 px-2 py-1.5 text-left"
        onClick={() => actions?.onSelectSpawn(spawn.spawnId)}
      >
        <span className="flex items-center gap-2">
          <span
            data-testid={`spawn-dot-${spawn.spawnId}`}
            data-status={spawn.status}
            className={`inline-block h-2 w-2 shrink-0 rounded-full ${DOT[spawn.status]}`}
          />
          {editing ? (
            <input
              data-testid={`spawn-name-input-${spawn.spawnId}`}
              autoFocus
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onClick={(e) => e.stopPropagation()}
              onKeyDown={(e) => {
                if (e.key === "Enter") commit();
                if (e.key === "Escape") setEditing(false);
              }}
              onBlur={commit}
              className="w-full rounded border border-input bg-background px-1 text-sm"
            />
          ) : (
            <span
              data-testid={`spawn-name-${spawn.spawnId}`}
              className="truncate text-sm font-medium"
              onDoubleClick={(e) => { e.stopPropagation(); startEdit(); }}
            >
              {spawn.name || spawn.appId}
            </span>
          )}
        </span>
        <span className="truncate pl-4 text-xs text-muted-foreground/70">{spawn.appId}</span>
      </button>

      <button
        data-testid={`spawn-kebab-${spawn.spawnId}`}
        className="px-2 py-1.5 text-muted-foreground hover:text-foreground"
        aria-label="spawn actions"
        onClick={() => { setMenu((m) => !m); setConfirmStop(false); }}
      >
        ⋯
      </button>

      {menu && (
        <div
          data-testid={`spawn-menu-${spawn.spawnId}`}
          className="absolute right-1 top-8 z-10 flex flex-col rounded-md border border-border bg-popover p-1 shadow-md"
        >
          <button data-testid={`spawn-rename-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={startEdit}>
            Rename
          </button>
          {spawn.status === "suspended" ? (
            <button data-testid={`spawn-resume-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={() => { setMenu(false); actions?.onResume(spawn.spawnId); }}>
              Resume
            </button>
          ) : (
            <button data-testid={`spawn-suspend-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm hover:bg-accent" onClick={() => { setMenu(false); actions?.onSuspend(spawn.spawnId); }}>
              Suspend
            </button>
          )}
          {confirmStop ? (
            <button data-testid={`spawn-stop-confirm-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm text-red-500 hover:bg-accent" onClick={() => { setMenu(false); setConfirmStop(false); actions?.onStop(spawn.spawnId); }}>
              Confirm stop
            </button>
          ) : (
            <button data-testid={`spawn-stop-${spawn.spawnId}`} className="rounded px-2 py-1 text-left text-sm text-red-500 hover:bg-accent" onClick={() => setConfirmStop(true)}>
              Stop
            </button>
          )}
        </div>
      )}
    </div>
  );
}
```

- [ ] **Step 4: Run the test + type-check** — `cd web && npx vitest run src/shell/Sidebar.test.tsx && npx tsc --noEmit` → PASS/clean.

- [ ] **Step 5: Commit**
```bash
git add web/src/shell/Sidebar.tsx web/src/shell/Sidebar.test.tsx
git commit --no-verify -m "feat(web): multi-spawn sidebar — list, status dots, kebab, inline rename (sp-r6b)"
```

---

### Task 3: App + AppShell — multi-spawn state, switch + history, lifecycle wiring

**Files:**
- Modify: `web/src/App.tsx` (full rewrite)
- Modify: `web/src/shell/AppShell.tsx` (full rewrite)
- Test: `web/src/shell/AppShell.test.tsx` (rewrite for the new props)

- [ ] **Step 1: Write the failing test** — replace `web/src/shell/AppShell.test.tsx` with:

```typescript
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { AppShell } from "./AppShell";
import type { SpawnView } from "@/api/spawnlet";

const baseProps = {
  status: "ready",
  items: [],
  busy: false,
  onSend: () => {},
  perm: null,
  onSpawnApp: vi.fn(),
};
const spawns: SpawnView[] = [{ spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active" }];
const actions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onStop: vi.fn() };

describe("AppShell", () => {
  it("renders the marketplace by default; chat not mounted", async () => {
    render(<AppShell {...baseProps} />);
    expect(screen.getByTestId("marketplace")).toBeTruthy();
    expect(screen.queryByTestId("prompt-input")).toBeNull();
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(screen.queryByTestId("marketplace")).toBeNull();
  });

  it("selecting a spawn navigates to chat and calls onSelectSpawn", async () => {
    render(<AppShell {...baseProps} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-select-a"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("a");
    expect(screen.getByTestId("prompt-input")).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run to verify it fails** — `cd web && npx vitest run src/shell/AppShell.test.tsx` → FAIL (AppShell doesn't accept `spawns`/`actions` yet; `spawn-select-a` absent).

- [ ] **Step 3: Rewrite `web/src/shell/AppShell.tsx`:**

```tsx
import { useState } from "react";
import { Toaster } from "@/components/ui/sonner";
import { Sidebar, type View, type SpawnActions } from "./Sidebar";
import { ChatView } from "@/views/ChatView";
import { MarketplaceView } from "@/views/MarketplaceView";
import { SettingsView } from "@/views/SettingsView";
import type { Item } from "@/views/chat/types";
import type { SpawnView } from "@/api/spawnlet";

export function AppShell({ status, items, busy, onSend, perm, onSpawnApp, spawns = [], activeId, actions }: {
  status: string;
  items: Item[];
  busy: boolean;
  onSend: (t: string) => void;
  perm: { title: string; resolve: (b: boolean) => void } | null;
  onSpawnApp: (appId: string) => void;
  spawns?: SpawnView[];
  activeId?: string | null;
  actions?: SpawnActions;
}) {
  const [view, setView] = useState<View>("market");
  const onSpawn = (appId: string) => { onSpawnApp(appId); setView("chat"); };
  // Selecting or resuming a spawn also navigates to its chat.
  const wrapped: SpawnActions | undefined = actions && {
    ...actions,
    onSelectSpawn: (id) => { actions.onSelectSpawn(id); setView("chat"); },
    onResume: (id) => { actions.onResume(id); setView("chat"); },
  };
  const activeName = spawns.find((s) => s.spawnId === activeId)?.name;
  return (
    <div className="flex h-screen bg-background text-foreground">
      <Sidebar view={view} onSelect={setView} spawns={spawns} activeId={activeId} actions={wrapped} />
      <div className="flex flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-4 py-2">
          <span className="text-sm font-medium">
            {view === "market" ? "Marketplace" : view === "settings" ? "Settings" : activeName || "Chat"}
          </span>
          <span data-testid="status" className="text-xs text-muted-foreground">{status}</span>
        </header>
        <main className="flex-1 overflow-hidden">
          {view === "chat" && <ChatView items={items} busy={busy} onSend={onSend} perm={perm} />}
          {view === "market" && <MarketplaceView onSpawn={onSpawn} />}
          {view === "settings" && <SettingsView />}
        </main>
      </div>
      <Toaster theme={typeof document !== "undefined" && document.documentElement.classList.contains("dark") ? "dark" : "light"} />
    </div>
  );
}
```

- [ ] **Step 4: Rewrite `web/src/App.tsx`:**

```tsx
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  createSpawn, listSpawns, renameSpawn, suspendSpawn, resumeSpawn, deleteSpawn,
  DEV_TOKEN, type SpawnView,
} from "./api/spawnlet";
import { Client, historyToItems } from "./acp/client";
import { AppShell } from "./shell/AppShell";
import { initialTheme, setTheme } from "./lib/theme";
import type { Item } from "./views/chat/types";

const MODEL = "openai/gpt-oss-120b:free";

export function App() {
  const [status, setStatus] = useState("");
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

  useEffect(() => { setTheme(initialTheme()); }, []);
  useEffect(() => { activeIdRef.current = activeId; }, [activeId]);
  useEffect(() => { itemsRef.current = items; }, [items]);
  useEffect(() => { spawnsRef.current = spawns; }, [spawns]);

  const withId = (it: Omit<Item, "id">): Item => ({ ...it, id: idRef.current++ } as Item);

  const refreshSpawns = async () => {
    try {
      const list = await listSpawns();
      setSpawns(list);
      if (activeIdRef.current && !list.some((s) => s.spawnId === activeIdRef.current)) {
        // active spawn disappeared from the ledger (stopped) — tear the live session down.
        genRef.current++;
        wsRef.current?.close();
        wsRef.current = null; clientRef.current = null;
        setActiveId(null); setItems([]); setStatus("");
      }
    } catch { /* transient; keep the last list */ }
  };

  useEffect(() => {
    refreshSpawns();
    const t = setInterval(refreshSpawns, 3000);
    return () => clearInterval(t);
  }, []);

  // On unmount just close the live ws — spawns persist on the node.
  useEffect(() => () => { wsRef.current?.close(); }, []);

  const closeSession = () => {
    genRef.current++;
    wsRef.current?.close();
    wsRef.current = null;
    clientRef.current = null;
  };

  const openSession = (spawnId: string) => {
    const gen = ++genRef.current;
    wsRef.current?.close();
    setBusy(true); setStatus("starting…");
    const ws = new WebSocket(`ws://${location.host}/ws/session`);
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    ws.onopen = async () => {
      ws.send(JSON.stringify({ spawnId, token: DEV_TOKEN }));
      const c = new Client(ws as any);
      clientRef.current = c;
      // CRI-lane adapter replays the transcript here; Docker lane never fires this (buffer is used).
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
        setStatus("error: " + e.message); setBusy(false);
        return;
      }
      if (genRef.current !== gen) return;
      setStatus("ready"); setBusy(false);
    };
    ws.onerror = () => { if (genRef.current !== gen) return; setStatus("connection error"); toast.error("Connection error"); };
    ws.onclose = () => { if (genRef.current !== gen) return; setStatus("session ended"); };
  };

  const spawnApp = async (appId: string) => {
    setBusy(true); setStatus("starting…");
    try {
      const id = await createSpawn(appId, MODEL);
      buffersRef.current.set(id, []);
      setItems([]);
      setActiveId(id);
      activeIdRef.current = id; // make active immediately for the onHistory guard
      await refreshSpawns();
      openSession(id);
    } catch (e: any) {
      setStatus("error: " + e.message); setBusy(false);
      toast.error("Spawn failed: " + e.message);
    }
  };

  const selectSpawn = (id: string) => {
    if (id === activeIdRef.current) return;
    if (activeIdRef.current) buffersRef.current.set(activeIdRef.current, itemsRef.current);
    closeSession();
    setActiveId(id);
    activeIdRef.current = id;
    const buf = buffersRef.current.get(id) ?? [];
    setItems(buf);
    const sp = spawnsRef.current.find((s) => s.spawnId === id);
    if (sp?.status === "active") openSession(id);
    else setStatus(sp?.status ?? "");
  };

  const onRename = async (id: string, name: string) => {
    setSpawns((xs) => xs.map((s) => (s.spawnId === id ? { ...s, name } : s))); // optimistic
    try { await renameSpawn(id, name); } catch (e: any) { toast.error("Rename failed: " + e.message); }
    refreshSpawns();
  };
  const onSuspend = async (id: string) => {
    try {
      await suspendSpawn(id);
      if (activeIdRef.current === id) { closeSession(); setStatus("suspended"); }
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
    if (activeIdRef.current === id) { closeSession(); setActiveId(null); activeIdRef.current = null; setItems([]); setStatus(""); }
    refreshSpawns();
  };

  const add = (it: Omit<Item, "id">) => setItems((xs) => [...xs, withId(it)]);
  const appendChunk = (kind: "agent" | "thought") => (t: string) =>
    setItems((xs) => {
      const last = xs[xs.length - 1];
      if (last && last.kind === kind) return [...xs.slice(0, -1), { ...last, text: last.text + t }];
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
      status={status}
      items={items}
      busy={busy}
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

Note: `appendChunk` narrows on `last.kind === kind` where `kind` is `"agent"|"thought"` — both those `Item` variants have `text`, so `last.text` is valid. `withId({ kind, text })` is fine because `{kind:"agent"|"thought", text}` is assignable to `Omit<Item,"id">`.

- [ ] **Step 5: Run the AppShell test + the FULL web unit suite + type-check**
```bash
cd web && npx vitest run src/shell/AppShell.test.tsx
cd web && npm test
cd web && npx tsc --noEmit
```
Expected: AppShell test PASS; full suite PASS (Sidebar/api/acp tests green); `tsc` clean. If any pre-existing test referenced the removed `activeSpawn` prop, it has already been updated in Task 2 (Sidebar.test) / this task (AppShell.test) — there should be no other references.

- [ ] **Step 6: Commit**
```bash
git add web/src/App.tsx web/src/shell/AppShell.tsx web/src/shell/AppShell.test.tsx
git commit --no-verify -m "feat(web): multi-spawn App state — switch+history buffer, lifecycle wiring (sp-r6b)"
```

---

### Task 4: Playwright e2e — multi-instance lifecycle + switch-history

**Files:**
- Modify: `web/e2e/chat.spec.ts` (update to the new sidebar selectors)
- Modify: `web/e2e/marketplace.spec.ts` (no spawn-selector changes needed; verify)
- Create: `web/e2e/spawn-lifecycle.spec.ts`

This task runs REAL containers; the sandbox has Docker. Rebuild the stub/sidecar images first (the adapter changed — `make images` is keyed on Go sources).

- [ ] **Step 1: Rebuild images + confirm the stack tooling**
```bash
cd /home/debian/AleCode/spawnery
make .make/img-stubagent .make/img-sidecar
```
Expected: both images build. (The e2e `global-setup.ts` builds the cp/spawnlet binaries itself and runs a self-hosted, floor-off node.)

- [ ] **Step 2: Update `web/e2e/chat.spec.ts`** — the helper used `nav-spawn`; the new sidebar uses per-spawn rows. Replace the helper + the second test's re-select:

Replace the `spawnFromMarketplace` helper body's final lines and the `nav-spawn` reference. The new helper:
```typescript
import { test, expect, type Page } from "@playwright/test";

// Spawn the seeded "Secret App" from the Marketplace; it lands in the sidebar Spawns list and the
// chat opens. Returns nothing — the active session is the just-spawned one.
async function spawnSecretApp(page: Page) {
  await page.goto("/");
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });
  // a spawn row appears in the sidebar (name defaults to the app id "secret-app").
  await expect(page.getByTestId("sidebar").getByText("secret-app", { exact: false })).toBeVisible();
}

test("chat echoes through the real browser", async ({ page }) => {
  await spawnSecretApp(page);
  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.getByTestId("prompt-input").fill("say " + token);
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="user"]')).toContainText(token);
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say " + token, { timeout: 30_000 });
});

test("settings theme toggle flips dark mode without dropping the session", async ({ page }) => {
  await spawnSecretApp(page);
  const html = page.locator("html");
  const wasDark = await html.evaluate((el) => el.classList.contains("dark"));
  await page.getByTestId("nav-settings").click();
  await page.getByTestId("theme-toggle").click();
  await expect.poll(() => html.evaluate((el) => el.classList.contains("dark"))).toBe(!wasDark);
  // return to the spawn (click its row) — the session is still live.
  await page.getByTestId("sidebar").getByText("secret-app", { exact: false }).first().click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 20_000 });
});
```

- [ ] **Step 3: Verify `web/e2e/marketplace.spec.ts` still matches** — it spawns `spawnery/wiki` and asserts `status` = ready + `prompt-input` visible. It does NOT use `nav-spawn`. Read it; if it references `nav-spawn` or `nav-chat`, update those to the new flow (it should not). No change expected.

- [ ] **Step 4: Create `web/e2e/spawn-lifecycle.spec.ts`:**

CRITICAL selector notes (these caused real bugs in review): (a) spawn the 2nd instance WITHOUT reloading the page — `page.goto("/")` wipes the client-side transcript buffer, breaking switch-history in the Docker lane; navigate via `nav-market` instead. (b) `filter({hasText})` is a SUBSTRING match, so "secret-app" would also match "secret-app 2" — use `filter({ has: page.getByText(name, { exact: true }) })` for exact name targeting (the name span text is exact, the app-id subline "spawnery/secret-app" is a different text node).

```typescript
import { test, expect, type Page } from "@playwright/test";

async function gotoApp(page: Page) {
  await page.goto("/");
  await expect(page.getByTestId("marketplace")).toBeVisible({ timeout: 20_000 });
}

// Spawn the seeded Secret App from the Marketplace WITHOUT reloading the page (preserves the
// client-side transcript buffer across instances). Assumes the app is already loaded (call gotoApp first).
async function spawnFromMarket(page: Page) {
  await page.getByTestId("nav-market").click();
  const card = page.getByTestId("app-card-spawnery/secret-app");
  await expect(card).toBeVisible({ timeout: 20_000 });
  await card.click();
  await expect(page.getByTestId("spawn-btn")).toBeVisible({ timeout: 10_000 });
  await page.getByTestId("spawn-btn").click();
  await expect(page.getByTestId("status")).toHaveText("ready", { timeout: 40_000 });
}

// the spawn-row whose name span has EXACTLY `name` (avoids the "secret-app" ⊂ "secret-app 2" trap).
function rowByName(page: Page, name: string) {
  return page.locator('[data-testid^="spawn-row-"]').filter({ has: page.getByText(name, { exact: true }) }).first();
}

test("two instances of the same app coexist with distinct names + active dots", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  await spawnFromMarket(page);
  // first instance "secret-app", second "secret-app 2" (server collision counter).
  await expect(rowByName(page, "secret-app 2")).toBeVisible({ timeout: 20_000 });
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(2);
  // both dots eventually report active (3 s ledger poll).
  await expect.poll(
    async () => page.locator('[data-testid^="spawn-dot-"][data-status="active"]').count(),
    { timeout: 20_000 },
  ).toBe(2);
});

test("rename a spawn from the sidebar", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  const r = rowByName(page, "secret-app");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-rename-"]').click();
  const input = r.locator('[data-testid^="spawn-name-input-"]');
  await input.fill("My Secret");
  await input.press("Enter");
  await expect(rowByName(page, "My Secret")).toBeVisible({ timeout: 10_000 });
});

test("suspend then resume a spawn", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  const r = rowByName(page, "secret-app");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-suspend-"]').click();
  await expect.poll(
    async () => r.locator('[data-testid^="spawn-dot-"]').getAttribute("data-status"),
    { timeout: 20_000 },
  ).toBe("suspended");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-resume-"]').click();
  await expect.poll(
    async () => r.locator('[data-testid^="spawn-dot-"]').getAttribute("data-status"),
    { timeout: 30_000 },
  ).toBe("active");
});

test("stop removes the spawn from the list", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page);
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(1);
  const r = rowByName(page, "secret-app");
  await r.locator('[data-testid^="spawn-kebab-"]').click();
  await r.locator('[data-testid^="spawn-stop-"]').click();          // arm confirm
  await r.locator('[data-testid^="spawn-stop-confirm-"]').click();  // confirm
  await expect(page.locator('[data-testid^="spawn-row-"]')).toHaveCount(0, { timeout: 20_000 });
});

test("switching between two spawns restores each transcript", async ({ page }) => {
  await gotoApp(page);
  await spawnFromMarket(page); // instance 1 active
  await page.getByTestId("prompt-input").fill("say one");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 30_000 });

  await spawnFromMarket(page); // instance 2 active (no reload → buffer for instance 1 preserved)
  await expect(page.locator('[data-role="agent"]')).toHaveCount(0);
  await page.getByTestId("prompt-input").fill("say two");
  await page.getByTestId("prompt-send").click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say two", { timeout: 30_000 });

  // switch back to instance 1 → its prior transcript is restored from the client buffer.
  await rowByName(page, "secret-app").locator('[data-testid^="spawn-select-"]').click();
  await expect(page.locator('[data-role="agent"]')).toContainText("ECHO: say one", { timeout: 20_000 });
  await expect(page.locator('[data-role="user"]')).toContainText("one");
});
```

- [ ] **Step 5: Run the e2e** — first ensure no stale stack is up, then run:
```bash
cd /home/debian/AleCode/spawnery
pkill -f 'bin/cp' 2>/dev/null; pkill -f 'bin/spawnlet' 2>/dev/null; just reap 2>/dev/null; rm -f cp.db; rm -rf .spawns
cd web && npm run test:e2e
```
Expected: all e2e specs pass (the 2 chat + 2 marketplace + 5 lifecycle). Generous timeouts absorb container cold starts. If a spec flakes on the 3 s ledger poll, the `expect.poll` timeouts (20–30 s) cover it.

- [ ] **Step 6: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/e2e/chat.spec.ts web/e2e/marketplace.spec.ts web/e2e/spawn-lifecycle.spec.ts
git commit --no-verify -m "test(web-e2e): multi-instance lifecycle + switch-history e2e (sp-r6b)"
```

---

## Definition of Done (Slice 3)

- Sidebar lists every spawn (name headline + app-id subline + status dot + kebab Rename/Suspend|Resume/Stop + inline rename); multiple instances of one app coexist (collision-counter names).
- App holds multi-spawn state with one live session for the active spawn; switching restores each spawn's transcript (client buffer); `onHistory` repopulates the active transcript when the adapter replays (CRI lane).
- Suspend/Resume/Stop/Rename wired to the CP RPCs; ledger status polled every 3 s drives the dots.
- Web unit suite + `tsc --noEmit` clean; Playwright e2e (incl. the 5 new lifecycle specs) green with real containers.
- `just dev` is usable for manual testing: spawn multiple Secret Apps, rename, suspend/resume, stop, switch between them.

## Out of scope
- Lossless suspend, durable/cross-reload history in the Docker lane (post-demo epics sp-3nb/sp-suc; the adapter replay covers the CRI lane).
- A confirm DIALOG (we use a two-click inline confirm in the kebab).
- Concurrent live sessions (one active at a time by design).
