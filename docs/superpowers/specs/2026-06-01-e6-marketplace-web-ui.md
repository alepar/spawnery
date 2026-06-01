# E6 Marketplace Web UI — Design

**Bead:** `sp-95v` (E6) — marketplace UI slice
**Status:** Draft v1 — autonomous track (user-chosen full scope)
**Date:** 2026-06-01
**Consumes:** the merged E5 catalog RPCs (ListApps/GetApp/CreateSpawn/ListMyApps/RegisterAppVersion/SetAppListing).

## 0. Context & constraints (from the existing `web/`)

React 19 + TS + Vite + Tailwind v4 + shadcn/ui. **Follow existing patterns** (decided, not re-litigated):
- **No react-router** — nested `useState` views (AppShell switches `chat|market|settings`).
- **Hand-rolled `fetch`** to `/cp.v1.SpawnService/{Method}` (Connect JSON, camelCase, `Authorization: Bearer dev-token`) — `api/spawnlet.ts`. The Vite proxy maps `/cp.v1.SpawnService/*` + `/ws/*` to the CP. No TS codegen (deferred).
- **shadcn primitives** (`Card`/`Button`/`Input`/`Badge`/`Dialog`/`Switch`) + `cn()` + semantic OKLCH tokens (no raw colors).
- **`data-testid` on every interactive element** (Playwright convention); Vitest for unit/component.

## 1. Scope (full, per user choice)

Replace the `MarketplaceView` placeholder with a 4-tab marketplace (nested state, no router):
1. **Browse** — `ListApps(query)` grid + search; tier badges; click → Detail.
2. **Detail** — `GetApp(id)`: manifest (title/description/model/tools/persona/agents) + versions table + tier; **Spawn** button → runs the app (wires into the existing chat spawn lifecycle).
3. **My Apps** — `ListMyApps()`: the owner's apps incl. unlisted; **takedown/relist** toggle (`SetAppListing`).
4. **Publish** — a form → `RegisterAppVersion` (structured manifest); on success → My Apps.

## 2. API client (`web/src/api/`)

- Extract the shared `unary<T>(method, body)` into `api/connect.ts` (re-export from `spawnlet.ts` to avoid breaking its imports, or move + update imports).
- New `api/catalog.ts` with TS types + functions:
  - Types: `TrustTier` (the Connect-JSON enum **string** form, e.g. `"TRUST_TIER_REVIEWED"`), `AppSummary` (`id, displayName, summary, tags[], latestVersion, latestTier, listed`), `AppVersionSummary` (`version, ref, tier, createdAt`), `AppManifest` (the fields we surface: `apiVersion, id, title, description, tags[], visibility, agents{support[],...}, tools[], persona, skills[], model{recommendedDefault,...}, mounts[]{name,path,seed}`).
  - `listApps(query=""): Promise<AppSummary[]>` → `ListApps` → `.apps ?? []`.
  - `getApp(id): Promise<{ app: AppSummary; versions: AppVersionSummary[]; manifest?: AppManifest }>`.
  - `listMyApps(): Promise<AppSummary[]>` → `.apps ?? []`.
  - `registerAppVersion(req: { manifest: AppManifest; version: string; ref: string }): Promise<{ appId; version; tier }>`.
  - `setAppListing(appId, listed): Promise<void>`.
  - `tierLabel(t?: TrustTier): { label: string; variant }` helper mapping the enum string → a short label (`reviewed`/`scanned`/`unverified`/`—`) + a Badge variant. **Connect JSON encodes proto enums as their NAME** — handle `TRUST_TIER_*` strings (and tolerate a missing/`UNSPECIFIED` value → `—`).

## 3. Spawn integration (the load-bearing refactor)

Today `App.tsx` auto-spawns `secret-app` in a mount `useEffect`. Refactor so a chosen app can be spawned on demand:
- Extract `async function spawnApp(appId: string, model: string)` from the effect: **tear down** any existing spawn (`stopSpawn` + `ws.close()`), reset `items`/`status`/`busy`, then create spawn + WS + ACP session (the current body).
- Keep the initial mount effect calling `spawnApp(APP_ID, MODEL)` (preserves the chat e2e: loads `secret-app` → `ready`).
- App passes `onSpawnApp={(appId) => spawnApp(appId, MODEL)}` to `AppShell`.
- `AppShell` owns `view`; it passes `onSpawn={(appId) => { onSpawnApp(appId); setView("chat"); }}` into `MarketplaceView`. So Detail's **Spawn** → re-spawns the chosen app + jumps to chat.
- Model: reuse the existing `MODEL` default for the demo (a per-spawn model picker is out of scope). Version: spawn **latest** (omit `version`); a version picker is optional/out.

> Guard the teardown so a re-spawn doesn't leak the old WS/spawn; the existing cleanup logic (alive flag, stopSpawn on unmount) is the template.

## 4. Views (nested state in `MarketplaceView`)

`MarketplaceView` holds `tab: "browse" | "detail" | "mine" | "publish"` + `selectedId: string | null`, receives `onSpawn(appId)` from AppShell. A small tab bar switches tabs; selecting a card sets `selectedId` + `tab="detail"`.

- **Browse:** search `Input` (debounced or on-submit) → `listApps(query)`; grid of `Card`s (`display_name`, `summary`, tag badges, `tierLabel` badge); empty/loading/error states. `data-testid="app-card-<id>"`.
- **Detail:** `getApp(selectedId)`; header (title + tier badge), description, manifest summary (model.recommendedDefault, tools, agents.support, persona path), a versions table (version / tier / createdAt), **Spawn** button (`data-testid="spawn-btn"`) → `onSpawn(app.id)`. Back button → browse.
- **My Apps:** `listMyApps()`; cards with a `listed` badge + a `Switch`/`Button` toggling `setAppListing(id, !listed)` (optimistic or refetch); empty state ("you haven't published any apps"). `data-testid="myapp-<id>"`, `data-testid="listing-toggle-<id>"`.
- **Publish:** form fields — `id` (creator/app), `title`, `description`, `tags` (csv), `version` (semver), `ref` (creator/app@sha), and a mounts mini-editor (rows of name/path/seed, default one `main`/`data`/`seed`). Defaults: `apiVersion="spawnery/v1"`, `visibility="open"`. On submit → `registerAppVersion({manifest, version, ref})`; success toast + go to My Apps; surface validation errors (the CP returns `InvalidArgument`/`PermissionDenied` — show the message via `sonner`). `data-testid="publish-form"`, `data-testid="publish-submit"`.

## 5. Testing

- **Vitest (hermetic):** `api/catalog.ts` (mock `fetch` — assert method, body, auth header, response mapping incl. `tierLabel`); component tests for Browse (renders cards from a mocked `listApps`), Detail (renders manifest+versions, Spawn calls `onSpawn`), My Apps (toggle calls `setAppListing`), Publish (submit builds the right manifest + calls `registerAppVersion`). Mock the `api/catalog` module.
- **Playwright e2e (`web/e2e/marketplace.spec.ts`):** browse → click app → detail → Spawn → chat shows `ready`. **May not run in the dev sandbox** (needs browsers + the spawnlet stack, like the existing chat e2e); write it to the existing pattern (`globalSetup` stack, `data-testid`, auto-wait) and run where supported. Don't weaken it to pass here.

## 6. Decision log

| # | Decision | Choice |
|---|---|---|
| W.1 | Routing | nested `useState` tabs in `MarketplaceView` (no react-router) |
| W.2 | API client | hand-rolled `fetch` via shared `unary` (extract to `api/connect.ts`); no codegen |
| W.3 | Enum encoding | Connect JSON enum **names** (`TRUST_TIER_*`); `tierLabel` maps to label+badge |
| W.4 | Spawn integration | extract `spawnApp(appId,model)` re-spawn in `App`; `onSpawn` thread via AppShell + `setView("chat")`; latest version, default model |
| W.5 | Publish form | structured fields (id/title/desc/tags/version/ref/mounts) + defaults (apiVersion, visibility=open); not a manifest-paste |
| W.6 | Tests | Vitest hermetic (api + components, mocked); Playwright e2e (host-gated, write-don't-weaken) |
| W.7 | Auth | existing `DEV_TOKEN`; OAuth deferred (E4) |
