# sp-jpn — Web UI: URL history + document.title reflect the current view

> Design agreed via brainstorming 2026-06-07. Folds in the Marketplace→Templates rename (sp-u5q).
> Routing layer: **wouter** (~2 kB). Title source: existing spawn name (no sp-ufz.3 dependency).

## Problem

The web SPA switches views via React `useState` only — no router, no browser-history
integration, static `document.title`. Consequences: Back/Forward don't work, views aren't
deep-linkable / shareable / refresh-stable, and the tab title never reflects context. Today the
navigable state is fragmented across three components:

- `shell/AppShell.tsx` — `view: "chat" | "market" | "settings"`
- `views/MarketplaceView.tsx` — `tab: "browse" | "detail" | "mine" | "publish"` + `selectedId`
- `App.tsx` — `activeId` (the selected spawn; drives the load-bearing ACP socket lifecycle)

## Goals

1. Push a history entry per navigable view (templates list, app detail, my-apps, publish, active
   spawn, settings).
2. Back/Forward (`popstate`) restore the corresponding view + sub-state.
3. Deep link / refresh lands on the right view (cold load parses the URL).
4. `document.title` reflects the current view.
5. Fold in the Marketplace→Templates rename (sp-u5q) so URLs/labels/titles use final names.

## Key decisions

- **URL is the single source of truth.** Collapse the three fragmented nav states into one `Nav`
  object derived from the path. Every navigation becomes a `navigate(nav)` call; a single
  reconciliation effect in `App` drives the imperative layer (ACP session open/close, transcript
  buffer swap, connection dot) off `nav` changes. This keeps the fragile socket/`genRef`/poll code
  intact — it's just re-pointed from "called on click" to "called on nav change," guarded so
  reconciliation never re-pushes the URL (no loop).
- **wouter** as the location layer (chosen over hand-rolled and react-router-dom): tiny, hooks-first
  (`useLocation`), gives `popstate`/Back-Forward + the history stack for free, without wrapping the
  tree in a heavy data-router or colliding with `App.tsx`'s imperative core.
- **Title source = existing spawn name** (`activeName`). Decouples from sp-ufz.3
  (agent-supplied conversation title), which becomes a later enhancement, not a blocker. The
  `sp-jpn → sp-ufz.3` dependency is removed.

## URL scheme & titles

| Path | View | `document.title` |
|---|---|---|
| `/` | normalize → `/templates` (replaceState) | — |
| `/templates` | Templates list (Browse) | `Spawnery — Templates` |
| `/templates/<appId>` | App detail | `Spawnery — <app title>` (fallback `<appId>` until catalog loads) |
| `/my-apps` | My Apps | `Spawnery — My Apps` |
| `/publish` | Publish | `Spawnery — Publish` |
| `/spawn/<spawnId>` | Chat **or** Terminal (by spawn mode) | `Spawnery — <spawn name>` (fallback `<appId>`/`<spawnId>`) |
| `/settings` | Settings | `Spawnery — Settings` |
| anything else | → `/templates` (replaceState) | — |

App detail nests under `/templates/<appId>` (the Templates hierarchy now owns it; cleaner than the
bead's tentative `/app/<id>`).

## Architecture

- `Nav` type: `{ section: "templates" | "app" | "my-apps" | "publish" | "spawn" | "settings",
  appId?: string, spawnId?: string }`.
- `src/nav/nav.ts` — two **pure** functions: `pathToNav(path): Nav` and `navToPath(nav): string`
  (round-trip; unknown path → `{section:"templates"}`).
- `src/nav/useNav.ts` — `useNav()` wrapping wouter's `useLocation()`: returns `[nav, navigate(nav)]`
  (parse on read, serialize + push on write).
- **Reconciliation effect** in `App`: reacts to `nav` changes, drives the imperative layer. The body
  of today's `selectSpawn` moves behind "nav changed to a different `spawnId`." Guard prevents the
  effect from re-pushing the URL.
- **Async spawn creation** stays imperative: `spawnApp(appId)` does the `createSpawn` round-trip,
  then `navigate({section:"spawn", spawnId:newId})` (the id isn't known until the round-trip
  returns).
- **Title effect** on `(nav, spawns, catalog)` sets `document.title`.

## Component changes

- `main.tsx` — wrap in wouter `<Router>` (default browser-history backend; no extra config).
- `App.tsx` — own `nav` via `useNav`; reconciliation effect; title effect; pass `nav` + `navigate`
  down; re-point `selectSpawn` / `spawnApp` / `onResume` through `navigate`.
- `AppShell.tsx` — drop local `view` useState; derive view + header label from `nav`.
- `MarketplaceView.tsx` → **`TemplatesView.tsx`** — drop local `tab`/`selectedId`; derive from
  `nav`; Browse→Detail and tab clicks call `navigate`.
- `Sidebar.tsx` — `View` enum `market`→`templates`; nav buttons + spawn rows call `navigate`.
- **sp-u5q rename (folded in):** user-facing "Marketplace"→"Templates", `nav-market`→`nav-templates`
  test ids, `MarketplaceView`→`TemplatesView` file/symbol, header strings. Closes sp-u5q.

## Testing

- **Vitest:** `nav.test.ts` round-trips every route + unknown→templates; title-effect test;
  AppShell / TemplatesView render-by-nav tests; update `AppShell.test.tsx` / `Sidebar.test.tsx` for
  renamed ids.
- **Playwright (`e2e/`):** update `marketplace.spec.ts` + `spawn-lifecycle.spec.ts` + `chat.spec.ts`
  to assert `page.url()` + `document.title` on navigation; add a deep-link/refresh test (cold-load
  `/spawn/<id>`) and a Back/Forward test. Dev refresh works via vite's SPA fallback.

## Out of scope / follow-ups

- **Prod static-serving fallback** (serve `index.html` for unknown paths) — not needed now (web is
  dev-only via vite, which already does SPA fallback). File a P3 follow-up for when `web/dist` gets
  served by Go.
- **Per-spawn tab deep links** (sp-npxq tab bar) — a clean later extension of `/spawn/<id>`; not in
  this bead.
