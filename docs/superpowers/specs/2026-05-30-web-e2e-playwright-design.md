# Web Browser E2E (Playwright vs. stub) — Design

**Status:** Approved in brainstorming — pending written-spec review
**Date:** 2026-05-30
**Context:** Real-browser e2e for the web ACP client
([web client design](2026-05-30-web-acp-client-design.md)). Complements the existing **Go WS e2e**
(transport + real agent) and the **`acp/` Vitest unit tests** by validating the actual **React UI +
browser WebSocket + Vite proxy** together, deterministically.

---

## 1. Goal & boundary

A headless **Chromium** (via **Playwright**) loads the web app, which spawns through the spawnlet
running the **stub agent** (`spawnery/stubagent:dev` — deterministic: echoes `ECHO: <text>`, **no
OpenRouter key, no model cost**). The test types a prompt and asserts the DOM renders the stub's
echo. This validates the real-browser integration path: React mount → `createSpawn` via the Vite
proxy → browser WebSocket → ACP handshake → message-chunk rendering → input → `stopSpawn` on close.

**Out of scope (the stub's coverage boundary):** the stub emits only a message chunk — **no
`tool_call`s, no permission requests** — so this does **not** browser-test the tool-call-chip or
permission-modal rendering. Those need a **live-Goose Playwright smoke** (real browser + key,
asserting `QUOKKA-4417` + the chip), noted as a thin optional extension (§7), not built here.

---

## 2. Orchestration (Playwright-native, self-contained)

`web/playwright.config.ts`:
- **`webServer`**: `npm run dev` (Vite on `:5173`; Playwright waits for it via `url`). The Vite proxy
  forwards `/spawn.v1.SpawnService/*` and `/ws/*` to the spawnlet.
- **`globalSetup`** (`web/e2e/global-setup.ts`): build the spawnlet binary and start it as a child
  process **from the repo root** (so the hardcoded relative `examples/secret-app` resolves) with:
  - `AGENT_IMAGE=spawnery/stubagent:dev`, `SIDECAR_IMAGE=spawnery/sidecar:dev`,
    `OPENROUTER_API_KEY=unused`, `DATA_ROOT=<repo>/.spawns`, `SPAWNLET_ADDR=127.0.0.1:9090`.
  - Wait until `:9090` accepts a TCP connection (poll, ~10s timeout). Store the child PID in a temp
    file for teardown.
- **`globalTeardown`** (`web/e2e/global-teardown.ts`): kill the spawnlet child (always — even on test
  failure).
- **project**: `chromium`, headless, `baseURL: http://localhost:5173`.

> `globalSetup`/`globalTeardown` are Node modules run by Playwright. They build the spawnlet via
> `go build -o <repo>/bin/spawnlet ./cmd/spawnlet` and spawn it with `child_process.spawn`.

---

## 3. The test (`web/e2e/chat.spec.ts`)

```
test("chat echoes through the real browser", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".status")).toHaveText("ready");      // app spawned + ACP handshake done
  const token = "ping-" + <unique>;                                // unguessable-by-accident
  await page.locator(".input textarea").fill("say " + token);
  await page.locator(".input button").click();
  await expect(page.locator(".bubble.user")).toContainText("say " + token);
  await expect(page.locator(".bubble.agent")).toContainText("ECHO: say " + token);
});
```
- Unique token per run so a pass cannot be accidental.
- Playwright **auto-waits** on the `ready` status and the agent bubble (no fixed sleeps) — absorbs the
  cold container start (~1–2s).
- Closing the page (Playwright context teardown) triggers the app's unmount → `stopSpawn`.

---

## 4. Files / packaging

```
web/package.json            MOD  add @playwright/test devDep + "test:e2e": "playwright test"
web/playwright.config.ts    NEW  webServer (vite) + globalSetup/Teardown + chromium project
web/e2e/global-setup.ts     NEW  build + start spawnlet (stub image), wait for :9090
web/e2e/global-teardown.ts  NEW  kill the spawnlet child
web/e2e/chat.spec.ts        NEW  the browser test
web/.gitignore              MOD  add playwright-report/ , test-results/
```
`npm run test:e2e` runs it. **Not** part of `npm test` (the hermetic Vitest unit run stays
Docker/browser-free).

---

## 5. Preconditions (documented; CI installs them)

- Docker available; **`spawnery/stubagent:dev` + `spawnery/sidecar:dev` images built**.
- `npx playwright install chromium` (browser binary) — done once.
- Go toolchain (globalSetup builds the spawnlet).
The test **fails loudly** (not skip) if the spawnlet can't start or `:9090` never opens — a broken
e2e env must be visible.

---

## 6. Error handling / flake resistance

- No fixed `sleep`s — Playwright auto-waiting on `.status=ready` and the agent bubble.
- `globalSetup` polls `:9090` with a timeout and throws (failing the run) if it never opens.
- `globalTeardown` kills the spawnlet child even when tests fail (try/finally around the PID kill).
- The spawn's container is torn down by the app's `stopSpawn` on page close; `globalTeardown` also
  best-effort prunes any `spawnery/*` container the run left (defensive).

---

## 7. Optional extension (not built here)

A **live-Goose Playwright smoke**: a second spec/project that runs `globalSetup` with
`AGENT_IMAGE=spawnery/goose:dev` + the key from `.env`, types "What is the secret word?", and asserts
the DOM shows the **tool-call chip** (reading `data/README.md`) + the **`QUOKKA-4417`** bubble. This is
the only thing that browser-tests the tool-chip rendering, but it's slow + keyed → a **manual /
pre-demo** run, gated to skip when `OPENROUTER_API_KEY` is unset. Tracked as a follow-up.

---

## Appendix — decision log

| # | Decision | Choice |
|---|---|---|
| P.1 | Agent for the e2e | **stub** (deterministic, no key/model); assert `ECHO: <unique token>` |
| P.2 | Orchestration | **Playwright-native** — `webServer` runs Vite; `globalSetup` builds+starts the spawnlet (stub image); `globalTeardown` kills it |
| P.3 | Packaging | `web/` + `@playwright/test`; `npm run test:e2e`; **not** in default `npm test` |
| P.4 | Flake strategy | auto-wait on `.status=ready` + the agent bubble; no fixed sleeps; teardown always kills |
| P.5 | Coverage boundary | core loop only (stub emits no tool_call/permission); live-Goose Playwright smoke = optional extension |
