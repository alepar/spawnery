# Web UI Framework Adoption — Design

**Date:** 2026-05-30
**Status:** Approved (design); ready for implementation plan
**Reference:** `web/FRONTEND_SETUP.md` (the bootstrap guide this adopts; committed for posterity)

## Goal

Adopt the prescribed frontend stack — React 19 + Tailwind CSS v4 + shadcn/ui (Radix),
with `react-virtuoso` + `streamdown` for the chat surface and a one-file semantic-token
theme — into the existing `web/` app **in place**, without breaking the ACP client, the
CP transport, the Vite proxy, the Vitest unit suite, or the Playwright browser e2e.

Scope (chosen): **foundation + chat migration + placeholder shells** for the future
sidebar / marketplace / settings surfaces. React **19** (bump from 18.3). Chat goes
**full guide**: virtualized + streaming-markdown + memoized rows.

## Background — where we are today

The `web/` app is React 18.3 + TypeScript + Vite, cleanly split:

- `src/acp/` — ACP protocol client (`client.ts`, `conn.ts`, `types.ts`) + Vitest unit tests
- `src/api/spawnlet.ts` — CP transport (`createSpawn`/`stopSpawn`, dev bearer token)
- `src/ui/` — hand-rolled chat components (`App`, `ChatLog`, `PromptInput`,
  `PermissionModal`, `Thoughts`, `ToolCallChip`) + `app.css` (hand-rolled CSS, no framework)
- `src/main.tsx` — `createRoot(...).render(<App/>)`
- `vite.config.ts` — `react()` plugin; `server.host: true`; proxy `/cp.v1.SpawnService`
  and `/ws` → CP `:8080`; Vitest `test` block excluding `e2e/**`
- `e2e/chat.spec.ts` — Playwright; drives the stub agent, asserts status `ready` and the
  `ECHO: say <token>` bubble. **Selects by CSS class** (`.status`, `.input textarea`,
  `.input button`, `.bubble.user`, `.bubble.agent`).
- `tsconfig.json` — single config, `moduleResolution: "bundler"`, `strict`, no path aliases

The stack the guide prescribes: Tailwind v4 (`@tailwindcss/vite`, no PostCSS), shadcn/ui
(Radix primitives, component source owned in `src/components/ui/`), semantic tokens in one
`globals.css` with a `.dark` class + no-flash `<head>` script, `react-virtuoso` (chat list),
`streamdown` (streaming markdown), `sonner` (toasts), and `clsx + tailwind-merge` via a
`cn()` helper with an `@` → `src` import alias.

## Approaches considered

1. **Adopt in place (CHOSEN).** Add the stack to the existing `web/` incrementally;
   restyle components against tokens. Preserves git history, the ACP/CP logic, and the
   proxy/test wiring. Only presentation changes.
2. **Scaffold fresh `web2/` from the guide, port logic, swap.** Matches the guide's
   greenfield bootstrap exactly but re-wires proxy/tests/e2e/ACP and risks divergence.
   Rejected: more churn, higher regression surface.
3. **Hybrid.** Scaffold fresh only in a throwaway temp dir to capture shadcn's exact
   generated config, then copy those files into `web/`. Adopted as an *implementation
   detail* of approach 1 (avoids a flaky interactive `shadcn init` in our non-standard
   layout) — config by reference, not guesswork.

## Architecture — session state above the view switch

The spawn/WebSocket/ACP lifecycle must survive view switches (clicking "Marketplace"
must NOT close the live chat socket). So that lifecycle stays at the top in `App`, and the
views are pure presentation siblings reading shared props:

```
App.tsx                  owns spawn lifecycle: createSpawn, WebSocket, ACP Client,
  │                      message state, onSend, appendChunk coalescing (logic UNCHANGED)
  └─ AppShell            shadcn Sidebar + content region; view: "chat"|"market"|"settings"
       ├─ Sidebar        nav (Chat / Marketplace / Settings) + placeholder "spawns" list
       ├─ ChatView       props {items, status, busy, onSend, perm}; renders migrated chat
       ├─ MarketplaceView placeholder: shadcn Card grid + "coming soon" empty state
       └─ SettingsView   placeholder + a REAL theme toggle (shadcn Switch flips `.dark`)
```

No `react-router` — view switching is local `useState` in `AppShell` (YAGNI; deferred
explicitly). The chat session persists regardless of which view renders.

### File map

| Path | Responsibility |
|---|---|
| `src/lib/utils.ts` | `cn()` — `twMerge(clsx(...))` |
| `src/lib/theme.ts` | `setTheme(t)` + init; reads/writes `localStorage["theme"]` |
| `src/globals.css` | the single token surface (replaces `app.css`) |
| `src/components/ui/*` | shadcn primitives — generated, committed as our source |
| `src/shell/AppShell.tsx` | sidebar + content region; holds `view` state |
| `src/shell/Sidebar.tsx` | nav + placeholder spawns list |
| `src/views/ChatView.tsx` | the chat surface (consumes session props) |
| `src/views/MarketplaceView.tsx` | placeholder card grid |
| `src/views/SettingsView.tsx` | placeholder + theme toggle |
| `src/views/chat/MessageList.tsx` | was `ChatLog`; Virtuoso + memo rows + Streamdown |
| `src/views/chat/PromptInput.tsx` | shadcn Input + Button |
| `src/views/chat/PermissionModal.tsx` | shadcn Dialog |
| `src/views/chat/Thoughts.tsx` | collapsible thought block |
| `src/views/chat/ToolCallChip.tsx` | tool-call chip (token-styled) |
| `src/App.tsx` | session wiring (moved from `src/ui/App.tsx`, logic kept verbatim) |
| `web/FRONTEND_SETUP.md` | the committed reference guide |

Deleted: `src/ui/app.css` and the old `src/ui/*` once their content has moved.

## Token architecture — guide's 3 layers, full shadcn contract

The guide's `globals.css` shows **7 illustrative tokens**; shadcn components reference ~25
(`--primary`, `--ring`, `--input`, `--card`, `--popover`, `--muted-foreground`,
`--destructive`, `--radius`, `--sidebar-*`, …). So `globals.css` keeps the guide's
**architecture** — `@import "tailwindcss"`, `@custom-variant dark (&:where(.dark, .dark *))`,
oklch values, off-black/off-white (never pure `#000`/`#fff`), `@theme inline` exposure, and
the font/size primitives (`--font-sans`, `--font-mono`, `--text-base`, `--leading-relaxed`)
— but populated with shadcn's **complete** token set for both `:root` (light) and `.dark`.

The no-flash script is inlined in `index.html` `<head>` **before** any stylesheet:
precedence explicit choice > system > light. `theme.ts`'s `setTheme` flips the `.dark`
class and writes the same `localStorage["theme"]` key the inline script reads.

## Chat surface — virtuoso + streamdown + memo

`MessageList` renders `<Virtuoso data={items} followOutput="smooth" />`. Each row is a
`React.memo` component keyed by a **stable per-item id** — `App` assigns an `id` when it
pushes/updates an item (the `appendChunk` coalescer updates the last item in place, so its
id is stable across streamed tokens). Agent and thought text render through `<Streamdown>`
(half-streamed tokens, code fences, GFM); user text is plain. Combined with row
memoization, a streaming token re-renders only the last row, not the whole transcript.
Assistant prose is measure-constrained to `max-w-[70ch]`. Tool-call chips and the
permission `Dialog` keep today's behavior and props.

`App`'s existing `appendChunk(kind)` coalescing logic is preserved verbatim; the only
change is that pushed items gain an `id` field.

## Error handling

- **Theme:** `localStorage` access wrapped in try/catch (per guide); falls back to system
  then light.
- **Connection / spawn errors:** today these surface only in the status text. With Sonner
  wired (`<Toaster>` mounted once in `AppShell`), `App`'s `ws.onerror` / spawn-create catch
  raise a toast in addition to setting status — so the dep earns its place.
- **Streaming:** `streamdown` is responsible for rendering partial/malformed markdown
  safely; no app-level guard needed.
- **Empty transcript:** Virtuoso renders nothing until the first item; the status banner
  carries `starting… → ready` state.

## Testing strategy — both suites stay green, fail-loud

- **Playwright (the real regression risk).** `chat.spec.ts`'s class selectors die with
  `app.css`. The migrated components expose **stable `data-testid` hooks** —
  `data-testid="status"`, `data-testid="prompt-input"`, `data-testid="prompt-send"`, and
  message rows carrying `data-role="user|agent|thought"`. `chat.spec.ts` is updated to use
  these; **assertions are unchanged** (`ready`, user bubble contains token, agent bubble
  contains `ECHO: say <token>`). One assertion is added: the sidebar nav renders and the
  Settings theme toggle flips `.dark` on `<html>`. No skips — the e2e fails loudly if the
  env breaks.
- **Vitest.** `src/acp/*` unit tests are untouched. The `@` → `src` alias is declared in
  `resolve.alias` so both Vite and Vitest resolve it. Add small pure-logic unit tests for
  `cn()` and `theme.ts`. No jsdom component-render tests — Virtuoso needs real layout
  measurement; that path is covered by Playwright.

## Tooling deltas

- **package.json:** `react`/`react-dom` → 19, `@types/react`/`@types/react-dom` → 19;
  add `tailwindcss`, `@tailwindcss/vite`, `clsx`, `tailwind-merge`, `react-virtuoso`,
  `streamdown`, `sonner` (+ the Radix deps shadcn pulls in); bump `@vitejs/plugin-react`.
  Verify peer ranges resolve on React 19; pin resolved versions.
- **vite.config.ts:** add `tailwindcss()` to `plugins` and `resolve.alias { "@": ./src }`.
  **Preserve** `server.host`, the proxy, and the Vitest `test` block verbatim.
- **tsconfig.json:** add `baseUrl: "."` and `paths: { "@/*": ["src/*"] }`.
- **index.html:** add the no-flash theme `<script>` in `<head>`.
- **shadcn:** generate `components.json` + token CSS in a throwaway temp dir, copy into
  `web/`, then `npx shadcn@latest add` the needed primitives:
  `button card dialog input switch tabs sidebar badge collapsible sonner`.
- **Justfile / `just setup`:** unchanged — `npm install` covers the new deps; `just web`
  stays `vite --host`.

## Out of scope (explicitly deferred)

- `react-router` / real client-side routing (local view state is enough now).
- Real Marketplace catalog and real Settings beyond the theme toggle.
- The command palette, context menus, dropdown-menu, select — pulled in when a surface
  needs them.
- Static-history `react-markdown` path (only the live-streaming `streamdown` path exists
  today; add `react-markdown` when persisted history lands).

## Success criteria

1. `npm run build` (tsc + vite) passes on React 19 with the new stack.
2. `npm test` (Vitest) green, including the new `cn()`/`theme.ts` tests.
3. `npm run test:e2e` (Playwright vs stub) green against the migrated UI via `data-testid`
   selectors, plus the new sidebar/theme-toggle assertion.
4. `just dev` brings up the restyled app; the secret-word click-through still works
   (status `ready`, tool chip, `QUOKKA-4417`) against real Goose.
5. Sidebar shows Chat/Marketplace/Settings; switching to Marketplace/Settings does NOT
   drop the live chat session; the Settings toggle flips light/dark with no flash on reload.
6. No component hardcodes a raw color — all styling reads semantic tokens.
