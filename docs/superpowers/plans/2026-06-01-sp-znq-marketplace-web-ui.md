# E6 Marketplace Web UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps. This is a **React/TS** plan — follow the existing `web/` patterns (shadcn + `cn()` + semantic tokens + `data-testid`); write idiomatic JSX (component tests are the spec gate, so exact markup isn't dictated).

**Goal:** A working marketplace in `web/`: browse/search apps, view detail + spawn, manage your apps (takedown/relist), and publish a version.

**Source spec:** `docs/superpowers/specs/2026-06-01-e6-marketplace-web-ui.md`. Bead `sp-znq`. Branch `sp-znq-market-ui` off master. Commits `--no-verify`.

**Run tests from `web/`:** `npm test` (Vitest, jsdom, hermetic). Build check: `npm run build` (tsc + vite). E2E: `npm run test:e2e` (Playwright — host-gated, may not run in sandbox). Existing component-test examples to mirror: `src/shell/Sidebar.test.tsx`, `src/views/SettingsView.test.tsx`, `src/views/chat/PromptInput.test.tsx`. Existing fetch pattern: `src/api/spawnlet.ts`.

---

## Task 1: API client — `api/connect.ts` + `api/catalog.ts` + unit tests

**Files:** Create `web/src/api/connect.ts`, `web/src/api/catalog.ts`, `web/src/api/catalog.test.ts`; modify `web/src/api/spawnlet.ts`.

- [ ] **Step 1: Extract the shared transport.** Create `web/src/api/connect.ts`:
```ts
// Calls the CP's ConnectRPC unary methods via plain fetch (Connect JSON, camelCase fields).
export const DEV_TOKEN = "dev-token";

export async function unary<T>(method: string, body: unknown): Promise<T> {
  const res = await fetch(`/cp.v1.SpawnService/${method}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      Authorization: `Bearer ${DEV_TOKEN}`,
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}
```
Update `web/src/api/spawnlet.ts` to import `{ unary, DEV_TOKEN }` from `./connect` and re-export `DEV_TOKEN` (so existing `import { DEV_TOKEN } from "./api/spawnlet"` in `App.tsx` keeps working): `export { DEV_TOKEN } from "./connect";` and delete the local `unary`/`DEV_TOKEN` definitions.

- [ ] **Step 2: Failing test** — create `web/src/api/catalog.test.ts`:
```ts
import { describe, it, expect, vi, beforeEach } from "vitest";
import { listApps, getApp, listMyApps, registerAppVersion, setAppListing, tierLabel } from "./catalog";

function mockFetch(json: unknown, ok = true) {
  return vi.fn().mockResolvedValue({ ok, status: ok ? 200 : 400, json: async () => json, text: async () => JSON.stringify(json) });
}

describe("catalog api", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("listApps posts query and maps apps", async () => {
    const f = mockFetch({ apps: [{ id: "a/b", displayName: "B", latestTier: "TRUST_TIER_REVIEWED", listed: true }] });
    vi.stubGlobal("fetch", f);
    const apps = await listApps("wiki");
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/ListApps", expect.objectContaining({ method: "POST" }));
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body).toEqual({ query: "wiki" });
    expect(apps[0].id).toBe("a/b");
  });

  it("listApps tolerates missing apps", async () => {
    vi.stubGlobal("fetch", mockFetch({}));
    expect(await listApps()).toEqual([]);
  });

  it("getApp returns app+versions+manifest", async () => {
    vi.stubGlobal("fetch", mockFetch({ app: { id: "a/b" }, versions: [{ version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" }], manifest: { id: "a/b", title: "B" } }));
    const r = await getApp("a/b");
    expect(r.app.id).toBe("a/b");
    expect(r.versions[0].version).toBe("1.0.0");
    expect(r.manifest?.title).toBe("B");
  });

  it("setAppListing posts appId+listed", async () => {
    const f = mockFetch({});
    vi.stubGlobal("fetch", f);
    await setAppListing("a/b", false);
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ appId: "a/b", listed: false });
  });

  it("registerAppVersion posts manifest+version+ref", async () => {
    const f = mockFetch({ appId: "a/b", version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" });
    vi.stubGlobal("fetch", f);
    const r = await registerAppVersion({ manifest: { apiVersion: "spawnery/v1", id: "a/b", title: "B", visibility: "open", mounts: [{ name: "main", path: "data", seed: "seed" }] } as any, version: "1.0.0", ref: "a/b@sha" });
    expect(r.tier).toBe("TRUST_TIER_UNVERIFIED");
  });

  it("listMyApps maps apps", async () => {
    vi.stubGlobal("fetch", mockFetch({ apps: [{ id: "a/b", listed: false }] }));
    expect((await listMyApps())[0].listed).toBe(false);
  });

  it("tierLabel maps enum names", () => {
    expect(tierLabel("TRUST_TIER_REVIEWED").label).toBe("reviewed");
    expect(tierLabel("TRUST_TIER_SCANNED").label).toBe("scanned");
    expect(tierLabel("TRUST_TIER_UNVERIFIED").label).toBe("unverified");
    expect(tierLabel(undefined).label).toBe("—");
  });
});
```

- [ ] **Step 3: Confirm failure:** `npm test -- catalog 2>&1 | tail -20` (module/exports missing).

- [ ] **Step 4: Implement `web/src/api/catalog.ts`:**
```ts
import { unary } from "./connect";

export type TrustTier =
  | "TRUST_TIER_UNSPECIFIED" | "TRUST_TIER_UNVERIFIED" | "TRUST_TIER_SCANNED" | "TRUST_TIER_REVIEWED";

export interface AppSummary {
  id: string;
  displayName?: string;
  summary?: string;
  tags?: string[];
  latestVersion?: string;
  latestTier?: TrustTier;
  listed?: boolean;
}
export interface AppVersionSummary { version: string; ref?: string; tier?: TrustTier; createdAt?: string; }
export interface ManifestMount { name: string; path: string; seed?: string; }
export interface AppManifest {
  apiVersion: string; id: string; title: string; description?: string; tags?: string[];
  visibility?: string;
  agents?: { support?: string[]; exclude?: string[]; requiresAcp?: string[] };
  tools?: string[]; persona?: string; skills?: string[];
  model?: { toolUse?: boolean; minContextTokens?: number; vision?: boolean; recommendedDefault?: string };
  runtimeBaseVersion?: string;
  mounts?: ManifestMount[];
}

export async function listApps(query = ""): Promise<AppSummary[]> {
  const r = await unary<{ apps?: AppSummary[] }>("ListApps", { query });
  return r.apps ?? [];
}
export async function getApp(id: string): Promise<{ app: AppSummary; versions: AppVersionSummary[]; manifest?: AppManifest }> {
  const r = await unary<{ app: AppSummary; versions?: AppVersionSummary[]; manifest?: AppManifest }>("GetApp", { id });
  return { app: r.app, versions: r.versions ?? [], manifest: r.manifest };
}
export async function listMyApps(): Promise<AppSummary[]> {
  const r = await unary<{ apps?: AppSummary[] }>("ListMyApps", {});
  return r.apps ?? [];
}
export async function registerAppVersion(req: { manifest: AppManifest; version: string; ref: string }): Promise<{ appId: string; version: string; tier: TrustTier }> {
  return unary("RegisterAppVersion", req);
}
export async function setAppListing(appId: string, listed: boolean): Promise<void> {
  await unary<Record<string, never>>("SetAppListing", { appId, listed });
}

export function tierLabel(t?: TrustTier): { label: string; variant: "default" | "secondary" | "outline" } {
  switch (t) {
    case "TRUST_TIER_REVIEWED": return { label: "reviewed", variant: "default" };
    case "TRUST_TIER_SCANNED":  return { label: "scanned", variant: "secondary" };
    case "TRUST_TIER_UNVERIFIED": return { label: "unverified", variant: "outline" };
    default: return { label: "—", variant: "outline" };
  }
}
```

- [ ] **Step 5:** `npm test -- catalog` PASS; `npm run build` clean (tsc). Confirm `App.tsx`'s `import { DEV_TOKEN } from "./api/spawnlet"` still resolves.

- [ ] **Step 6: Commit:** `git add web/src/api && git commit --no-verify -m "feat(web): catalog API client + shared connect transport (sp-znq)"`

---

## Task 2: Spawn-integration refactor (`App.tsx` + `AppShell.tsx`)

**Files:** Modify `web/src/App.tsx`, `web/src/shell/AppShell.tsx`; create `web/src/shell/AppShell.test.tsx` (or extend Sidebar test).

- [ ] **Step 1:** In `App.tsx`, extract the spawn body into `spawnApp(appId, model)`:
  - Move the current effect's body into `const spawnApp = async (appId: string, model: string) => { ... }` that FIRST tears down any existing spawn: `wsRef.current?.close(); if (spawnRef.current) stopSpawn(spawnRef.current); spawnRef.current = ""; setItems([]); setBusy(true); setStatus("starting…");` then runs the existing create→ws→ACP logic (using `appId`/`model`).
  - Keep an `alive`-style guard appropriate for an on-demand call (the mount effect's cleanup still closes the ws on unmount). Simplest: keep a module-level `aliveRef`/generation counter so a stale ws callback from a previous spawn doesn't clobber state — use a `genRef = useRef(0)`, increment at the start of `spawnApp`, and in ws callbacks check the captured gen still matches before `setStatus`/`setBusy`.
  - The mount `useEffect` becomes `useEffect(() => { spawnApp(APP_ID, MODEL); return () => { wsRef.current?.close(); if (spawnRef.current) stopSpawn(spawnRef.current); }; }, [])`.
- [ ] **Step 2:** Pass `onSpawnApp={(appId: string) => spawnApp(appId, MODEL)}` to `<AppShell ... />`.
- [ ] **Step 3:** In `AppShell.tsx`, add `onSpawnApp: (appId: string) => void` to the props; pass `onSpawn={(appId) => { onSpawnApp(appId); setView("chat"); }}` into `<MarketplaceView onSpawn={onSpawn} />`. (MarketplaceView gains the `onSpawn` prop in Task 3; until then a stub prop type is fine — or do Task 3 first. Run tasks in order: this task wires AppShell→MarketplaceView, so MarketplaceView must accept `onSpawn?: (appId: string) => void`. Add that optional prop to the current placeholder MarketplaceView signature here so the build stays green.)
- [ ] **Step 4: Test** — create `web/src/shell/AppShell.test.tsx` (mirror `Sidebar.test.tsx`): render `<AppShell>` with a spy `onSpawnApp`; it renders the chat header by default; clicking the Marketplace sidebar button shows the marketplace testid. (A deeper "Spawn switches to chat" assertion comes with Task 3's Detail; here just assert the prop is wired + views switch.)
- [ ] **Step 5:** `npm test` (all existing + new pass — esp. don't break `Sidebar.test.tsx`); `npm run build` clean.
- [ ] **Step 6: Commit:** `git add web/src/App.tsx web/src/shell && git commit --no-verify -m "feat(web): on-demand spawnApp + onSpawn wiring through AppShell (sp-znq)"`

---

## Task 3: Browse + Detail views (+ Spawn)

**Files:** Modify `web/src/views/MarketplaceView.tsx`; create `web/src/views/market/Browse.tsx`, `web/src/views/market/Detail.tsx`, and tests `web/src/views/market/Browse.test.tsx`, `Detail.test.tsx`.

- [ ] **Step 1:** Rebuild `MarketplaceView` as a tabbed shell: props `{ onSpawn?: (appId: string) => void }`; state `tab: "browse"|"detail"|"mine"|"publish"` (default "browse") + `selectedId: string|null`. A tab bar (shadcn `Button`s, `data-testid="market-tab-<tab>"`). Render `<Browse onOpen={(id)=>{setSelectedId(id);setTab("detail")}} />`, `<Detail id={selectedId!} onBack={()=>setTab("browse")} onSpawn={onSpawn} />` (when tab==="detail" && selectedId), and the Task-4 tabs.
- [ ] **Step 2: Browse** (`market/Browse.tsx`): a search `Input` (`data-testid="market-search"`) + a grid of `Card`s. On mount + on search submit, call `listApps(query)`; render loading/empty/error. Each card: `display_name`, `summary`, tag `Badge`s, a `tierLabel(latestTier)` `Badge`; `data-testid="app-card-<id>"`; click → `onOpen(id)`. Test (`Browse.test.tsx`): `vi.mock("@/api/catalog", ...)` returning two apps → assert two cards render + clicking one calls `onOpen` with the id (use `@testing-library/react` + `user-event`, mirror `PromptInput.test.tsx`).
- [ ] **Step 3: Detail** (`market/Detail.tsx`): on mount call `getApp(id)`; render header (`display_name`/title + `tierLabel` badge), description, a manifest summary (model.recommendedDefault, tools join, agents.support join, persona), a versions table (version / tier / createdAt), a **Spawn** `Button` (`data-testid="spawn-btn"`) → `onSpawn?.(id)`, and a Back button → `onBack`. Test (`Detail.test.tsx`): mock `getApp` → assert manifest fields + versions render; click Spawn → `onSpawn` called with the id.
- [ ] **Step 4:** `npm test` PASS (browse+detail+existing); `npm run build` clean.
- [ ] **Step 5: Commit:** `git add web/src/views && git commit --no-verify -m "feat(web): marketplace Browse + Detail views with Spawn (sp-znq)"`

---

## Task 4: My Apps + Publish views

**Files:** Create `web/src/views/market/MyApps.tsx`, `web/src/views/market/Publish.tsx`, tests `MyApps.test.tsx`, `Publish.test.tsx`; wire both tabs into `MarketplaceView`.

- [ ] **Step 1: My Apps** (`market/MyApps.tsx`): on mount `listMyApps()`; cards (`data-testid="myapp-<id>"`) showing `display_name`, `tierLabel`, and a `listed` badge; a toggle control (`data-testid="listing-toggle-<id>"`) calling `setAppListing(id, !listed)` then refetching (or optimistic). Empty state. Test: mock `listMyApps` (one listed, one unlisted) → both render with correct listed state; toggling calls `setAppListing(id, expected)`.
- [ ] **Step 2: Publish** (`market/Publish.tsx`): form (`data-testid="publish-form"`) with `Input`s for `id`, `title`, `description`, `tags` (csv), `version`, `ref`, and a mounts editor (default one row name=`main`/path=`data`/seed=`seed`; add/remove rows). Submit (`data-testid="publish-submit"`) builds an `AppManifest` (`apiVersion:"spawnery/v1"`, `visibility:"open"`, `tags` split on comma, mounts from rows) and calls `registerAppVersion({manifest, version, ref})`; on success `toast.success` + (if a tab callback is passed) switch to My Apps; on error `toast.error(e.message)`. Test: fill fields (via `user-event`), mock `registerAppVersion` resolved → assert it's called with the assembled manifest (id/title/version/ref/mounts) and success toast path; mock rejected → error path doesn't throw.
- [ ] **Step 3:** Wire `MarketplaceView` tabs "mine" → `<MyApps />` and "publish" → `<Publish onPublished={()=>setTab("mine")} />`.
- [ ] **Step 4:** `npm test` PASS; `npm run build` clean.
- [ ] **Step 5: Commit:** `git add web/src/views && git commit --no-verify -m "feat(web): My Apps (takedown/relist) + Publish form (sp-znq)"`

---

## Task 5: Playwright marketplace e2e (host-gated)

**Files:** Create `web/e2e/marketplace.spec.ts`.

> Mirror `web/e2e/chat.spec.ts` (uses `global-setup.ts` to bring up the spawnlet+stub stack; `data-testid` selectors; auto-wait). This needs browsers + the stack — **may not run in the dev sandbox**; write it to the pattern, run where supported, do NOT weaken.

- [ ] **Step 1:** Write `marketplace.spec.ts`: load the app → click the Marketplace sidebar/tab → wait for `app-card-*` to populate (the CP seed lineup) → click a card → assert Detail renders (title + `spawn-btn`) → click `spawn-btn` → assert it switches to chat and `status` reaches `ready`.
- [ ] **Step 2: Compile/lint check only:** `cd web && npx tsc --noEmit -p tsconfig.json` (the spec is TS; ensure it type-checks). Do NOT assume `npm run test:e2e` runs here; if it can't reach browsers/stack it's expected to fail — that's host-gated, not a bug.
- [ ] **Step 3: Commit:** `git add web/e2e && git commit --no-verify -m "test(web): marketplace browse→detail→spawn e2e (host-gated) (sp-znq)"`

---

## Final Verification
- [ ] `cd web && npm test` — all Vitest suites pass (api + components + existing).
- [ ] `cd web && npm run build` — tsc + vite clean.
- [ ] `cd /home/debian/AleCode/spawnery && go build ./...` — backend untouched, still builds.

Then **superpowers:finishing-a-development-branch** (Option 1: merge locally). Note at merge: the Playwright marketplace e2e is host-gated (browsers + spawnlet stack), unverified in the sandbox; Vitest hermetic coverage is green.

---

## Self-Review Notes
- **Spec coverage:** §2 api → T1; §3 spawn integration → T2; §4 Browse/Detail → T3, MyApps/Publish → T4; §5 tests → each task + T5. ✓
- **Backward-compat:** `DEV_TOKEN` re-exported from `spawnlet.ts` (App.tsx import unchanged); existing Vitest suites must stay green (Sidebar/Settings/PromptInput). ✓
- **Enum handling:** `tierLabel` maps Connect-JSON enum names; tolerant of missing. ✓
- **Spawn re-entrancy:** `genRef` guard prevents a stale ws callback from a prior spawn clobbering the new one. ✓
