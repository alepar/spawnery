# Web Browser E2E (Playwright vs. stub) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Playwright headless-Chromium e2e that loads the React web app, lets it spawn through the spawnlet running the **stub agent** (deterministic — no key/model), types a prompt, and asserts the DOM renders the stub's echo.

**Architecture:** Playwright-native orchestration in `web/`: `webServer` runs `npm run dev` (Vite); `globalSetup` builds + starts the spawnlet child process with `AGENT_IMAGE=spawnery/stubagent:dev` and waits for `:9090`; `globalTeardown` kills it. One spec drives the browser and asserts `ECHO: <token>`.

**Tech Stack:** `@playwright/test` (Chromium), Node 20, the existing Vite app + Go spawnlet.

**Spec:** `docs/superpowers/specs/2026-05-30-web-e2e-playwright-design.md` (authoritative).

> **Note on TDD:** the Playwright spec *is* the test deliverable; there's no "failing unit test for the test." Granularity here is: build the config + setup/teardown + spec, then **run it once and confirm it passes** (Task 4) — that run is the red→green gate.

---

## Conventions
- Branch: `feat/web-e2e-playwright`. Beads `sp` milestone in_progress at Task 1, close after Task 4. No TodoWrite.
- Run all web commands from `web/` (`cd /home/debian/AleCode/spawnery/web && …`).
- Commit per task; Co-Authored-By trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. No git remote — commit only.
- Preconditions for the run (Task 4): Docker up; `spawnery/stubagent:dev` + `spawnery/sidecar:dev` images built; `npx playwright install chromium`.

## File Structure
```
web/package.json            MOD  + @playwright/test devDep, + "test:e2e" script
web/.gitignore              MOD  + playwright-report/  + test-results/
web/playwright.config.ts    NEW  webServer(vite) + chromium + globalSetup/Teardown
web/e2e/global-setup.ts     NEW  build + start spawnlet (stub image), wait for :9090
web/e2e/global-teardown.ts  NEW  kill the spawnlet child
web/e2e/chat.spec.ts        NEW  the browser test
```

---

## Task 1: Playwright dep + scripts + gitignore

**Files:** Modify `web/package.json`, `web/.gitignore`.

- [ ] **Step 1: Add the dep + script**

Run:
```bash
cd /home/debian/AleCode/spawnery/web && npm install -D @playwright/test@^1.49.0
```
Then add to `web/package.json` `"scripts"` (alongside dev/build/test):
```json
    "test:e2e": "playwright test"
```

- [ ] **Step 2: gitignore Playwright artifacts** — append to `web/.gitignore`:
```
playwright-report/
test-results/
```

- [ ] **Step 3: Install the browser** (one-time; needed for Task 4's run):
```bash
cd /home/debian/AleCode/spawnery/web && npx playwright install chromium
```
Expected: chromium downloaded.

- [ ] **Step 4: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/package.json web/package-lock.json web/.gitignore
git commit -m "chore(web): add @playwright/test + test:e2e script

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: playwright.config.ts

**Files:** Create `web/playwright.config.ts`.

- [ ] **Step 1: Write the config** — `web/playwright.config.ts`:
```ts
import { defineConfig, devices } from "@playwright/test";

// Self-contained: Playwright starts Vite (webServer) and the spawnlet
// (globalSetup, configured to the STUB agent — deterministic, no key/model).
export default defineConfig({
  testDir: "./e2e",
  globalSetup: "./e2e/global-setup.ts",
  globalTeardown: "./e2e/global-teardown.ts",
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  use: {
    baseURL: "http://localhost:5173",
    headless: true,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "npm run dev",
    url: "http://localhost:5173",
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
});
```
> `testDir: ./e2e` — Playwright's default `testMatch` is `*.spec.ts`/`*.test.ts`, so `global-setup.ts`/`global-teardown.ts` are NOT picked up as tests. `workers: 1` + `fullyParallel: false` because there's one shared spawnlet.

- [ ] **Step 2: Verify it parses** — `cd /home/debian/AleCode/spawnery/web && npx playwright test --list 2>&1 | head`
Expected: it lists `e2e/chat.spec.ts` once that file exists (Task 4); for now it may report "no tests found" — that's fine (no error parsing the config). If the config has a syntax error, this surfaces it.

- [ ] **Step 3: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/playwright.config.ts
git commit -m "test(web/e2e): Playwright config (vite webServer + chromium)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: globalSetup + globalTeardown (start/stop the spawnlet w/ stub)

**Files:** Create `web/e2e/global-setup.ts`, `web/e2e/global-teardown.ts`.

- [ ] **Step 1: global-setup** — `web/e2e/global-setup.ts`:
```ts
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(__dirname, "..", ".."); // web/e2e -> repo root
const PID_FILE = path.join(os.tmpdir(), "spawnery-e2e-spawnlet.pid");

function run(cmd: string, args: string[], cwd: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { cwd, stdio: "inherit" });
    p.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`${cmd} exited ${code}`))));
    p.on("error", reject);
  });
}

function waitForPort(host: string, port: number, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const s = net.connect(port, host);
      s.once("connect", () => { s.destroy(); resolve(); });
      s.once("error", () => {
        s.destroy();
        if (Date.now() > deadline) reject(new Error(`spawnlet did not open ${host}:${port} in ${timeoutMs}ms`));
        else setTimeout(tick, 250);
      });
    };
    tick();
  });
}

export default async function globalSetup() {
  // 1. build the spawnlet binary
  await run("go", ["build", "-o", path.join(REPO, "bin", "spawnlet"), "./cmd/spawnlet"], REPO);

  // 2. start it with the STUB agent image (deterministic; no key needed)
  const child = spawn(path.join(REPO, "bin", "spawnlet"), [], {
    cwd: REPO, // so the hardcoded relative examples/secret-app resolves
    env: {
      ...process.env,
      AGENT_IMAGE: "spawnery/stubagent:dev",
      SIDECAR_IMAGE: "spawnery/sidecar:dev",
      OPENROUTER_API_KEY: "unused",
      DATA_ROOT: path.join(REPO, ".spawns"),
      SPAWNLET_ADDR: "127.0.0.1:9090",
    },
    stdio: "inherit",
  });
  writeFileSync(PID_FILE, String(child.pid));

  // 3. wait until it's listening, else FAIL the run (no silent skip)
  await waitForPort("127.0.0.1", 9090, 15_000);
}
```

- [ ] **Step 2: global-teardown** — `web/e2e/global-teardown.ts`:
```ts
import { readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";

const PID_FILE = path.join(os.tmpdir(), "spawnery-e2e-spawnlet.pid");

export default async function globalTeardown() {
  try {
    const pid = parseInt(readFileSync(PID_FILE, "utf8").trim(), 10);
    if (pid) {
      try { process.kill(pid, "SIGTERM"); } catch { /* already gone */ }
    }
  } finally {
    try { rmSync(PID_FILE); } catch { /* ignore */ }
  }
}
```
> Note (documented limitation): killing the spawnlet does not stop its Docker containers; a test spawn whose `stopSpawn` didn't complete before page-close may leave a stub/sidecar container. This is the known orphan case (`sp-8hf`); clean stragglers with `docker container prune` if they accumulate.

- [ ] **Step 3: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/e2e/global-setup.ts web/e2e/global-teardown.ts
git commit -m "test(web/e2e): global setup/teardown — spawnlet w/ stub agent

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: The browser test + run it green

**Files:** Create `web/e2e/chat.spec.ts`; modify `README.md`.

- [ ] **Step 1: The spec** — `web/e2e/chat.spec.ts`:
```ts
import { test, expect } from "@playwright/test";

test("chat echoes through the real browser", async ({ page }) => {
  await page.goto("/");

  // App spawns on mount + runs the ACP handshake; status flips to "ready".
  // Generous timeout absorbs the container cold start.
  await expect(page.locator(".status")).toHaveText("ready", { timeout: 40_000 });

  const token = "ping-" + Math.random().toString(36).slice(2, 8);
  await page.locator(".input textarea").fill("say " + token);
  await page.locator(".input button").click();

  // user echo bubble + the stub's "ECHO: <prompt>" agent bubble.
  await expect(page.locator(".bubble.user")).toContainText(token);
  await expect(page.locator(".bubble.agent")).toContainText("ECHO: say " + token, { timeout: 30_000 });
});
```

- [ ] **Step 2: Ensure images exist** (Task-4 run prerequisite):
```bash
cd /home/debian/AleCode/spawnery
docker image inspect spawnery/stubagent:dev >/dev/null 2>&1 || docker build -t spawnery/stubagent:dev -f deploy/stubagent/Dockerfile .
docker image inspect spawnery/sidecar:dev   >/dev/null 2>&1 || docker build -t spawnery/sidecar:dev   -f deploy/sidecar/Dockerfile .
```

- [ ] **Step 3: Run the e2e** (the red→green gate):
```bash
cd /home/debian/AleCode/spawnery/web && npm run test:e2e
```
Expected: PASS — `1 passed`. Playwright starts Vite + the spawnlet (stub image), loads the app in headless Chromium, the status reaches `ready`, the prompt is sent, and the DOM shows `ECHO: say ping-XXXXXX`. If it fails, debug for real: check the spawnlet logs in the Playwright output, the `.status` selector, the Vite proxy, the WS connect (`/ws/session`). Do NOT weaken the assertion to pass. After the run, ensure no leftover containers: `docker ps`.

- [ ] **Step 4: Document** — add a short "### Browser e2e" subsection under the README's web-client section: the one-time `npx playwright install chromium`, that images must be built, and `cd web && npm run test:e2e`. Note it uses the stub agent (deterministic, no key).

- [ ] **Step 5: Commit**
```bash
cd /home/debian/AleCode/spawnery
git add web/e2e/chat.spec.ts README.md
git commit -m "test(web/e2e): browser chat e2e vs stub (ECHO round-trip) + docs

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** §1 goal (real browser → app → stub spawn → assert echo) → Tasks 3,4; §2 orchestration (webServer vite + globalSetup spawnlet stub + globalTeardown) → Tasks 2,3; §3 the test (status=ready, fill token, assert ECHO) → Task 4; §4 packaging (`@playwright/test`, `test:e2e`, not in `npm test`, e2e files + gitignore) → Tasks 1,2,3,4; §5 preconditions (Docker+images+`playwright install`, fail-loud) → Task 1 Step 3 + Task 4 Step 2 + globalSetup `waitForPort` throws; §6 flake resistance (auto-wait, no sleeps, teardown always kills) → Tasks 3,4. **No gaps.** §7 (live-Goose Playwright smoke) is explicitly out of scope.

**Placeholder scan:** none — every file has complete code; the unique token uses `Math.random` (allowed in a Playwright test, unlike workflow scripts). The only documented caveat is the container-leak note (teardown kills the spawnlet, not its containers — tracked `sp-8hf`).

**Type consistency:** selectors `.status` (toHaveText "ready"), `.bubble.user`, `.bubble.agent`, `.input textarea`, `.input button` all match the merged web client's `app.css`/components. `PID_FILE` path identical in setup + teardown (`os.tmpdir()/spawnery-e2e-spawnlet.pid`). Env vars (`AGENT_IMAGE`/`SIDECAR_IMAGE`/`OPENROUTER_API_KEY`/`DATA_ROOT`/`SPAWNLET_ADDR`) match `cmd/spawnlet/main.go`.

---

## Beads
One milestone: `Web browser e2e (Playwright vs stub)`. Mark in_progress at Task 1; close after Task 4 passes. Part of E6 (`sp-95v`).
