/**
 * CSP-enforced prod-bundle Playwright suite (W1, sp-2ckv.6, [WM19]).
 *
 * Exercises the real prod build under enforced CSP to catch dependency regressions
 * that introduce eval/inline JS or undocumented style injection. Runs against
 * `vite preview` (dist/ built with `npm run build`).
 *
 * Skip conditions:
 *  - PLAYWRIGHT_BROWSERS_UNAVAILABLE is set (CI without browsers).
 *  - dist/ does not exist (no prod build).
 *
 * Each test collects CSP violation console errors and fails if any are found,
 * EXCEPT the documented style-src 'unsafe-inline' violations from xterm and sonner
 * (which are expected under a strict CSP and are covered by 'unsafe-inline' in the
 * production _headers — see deploy/web/README.md §CSP for the decision record).
 *
 * If a test fails with a CSP violation for script-src or default-src, a dependency
 * has regressed and must be fixed before merging.
 */

import { test, expect } from "@playwright/test";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DIST_DIR = path.resolve(__dirname, "../dist");

/** Compute the sha384 integrity value over the raw bytes of a dist file. */
function sha384FromDisk(filePath: string): string {
  const buf = fs.readFileSync(filePath);
  return "sha384-" + crypto.createHash("sha384").update(buf).digest("base64");
}

// Skip the entire suite if no prod build exists or browsers are unavailable.
const skipReason = !fs.existsSync(DIST_DIR)
  ? "no dist/ — run `npm run build` first"
  : process.env.PLAYWRIGHT_BROWSERS_UNAVAILABLE
  ? "browsers unavailable"
  : null;

test.skip(() => skipReason !== null, skipReason ?? "");

test.describe("CSP-enforced prod bundle", () => {
  test("page loads without script-src CSP violations", async ({ page }) => {
    const violations: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() !== "error") return;
      const text = msg.text();
      // Capture both CSP policy violations AND SRI integrity mismatches. SRI errors say
      // "Failed to find a valid digest in the integrity attribute" — no "Content Security
      // Policy" substring — so they must be collected separately.
      const isCspOrSri =
        text.includes("Content Security Policy") ||
        text.includes("Failed to find a valid digest") ||
        text.includes("integrity attribute");
      if (isCspOrSri && !text.includes("style-src")) {
        violations.push(text);
      }
    });

    // Silence background polls so networkidle resolves promptly.
    await page.route("**/cp.v1.SpawnService/**", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: '{}' })
    );

    await page.goto("/");
    await page.waitForLoadState("networkidle");

    // Assert the app root rendered — an SRI mismatch would block the entry script and
    // leave #root empty; React mounting proves the bundle was accepted by the browser.
    await expect(page.locator("#root > *")).toBeAttached({ timeout: 5000 });

    expect(violations, `CSP/SRI violations:\n${violations.join("\n")}`).toHaveLength(0);
  });

  test("fonts load without CSP violations (terminal font render) [WM19]", async ({ page }) => {
    const violations: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        if (!msg.text().includes("style-src")) violations.push(msg.text());
      }
    });

    await page.goto("/");
    await page.waitForLoadState("networkidle");

    // Verify fonts are loaded (not blocked by font-src).
    const fontsLoaded = await page.evaluate(() =>
      document.fonts.ready.then(() => document.fonts.size > 0)
    );
    expect(fontsLoaded).toBeTruthy();
    expect(violations).toHaveLength(0);
  });

  test("highlight/diagram block renders without CSP violations (highlighted-body chunk) [WM19]", async ({ page }) => {
    // This test exercises the shiki/streamdown render path — the precise dep that WM19 guards
    // against eval/inline/wasm regressions. It navigates to an ACP chat session, injects an
    // agent message with a fenced code block via a mocked WebSocket, and waits for the
    // highlighted-body chunk to execute (HighlightedCodeBlockBody mounts and calls shiki).
    // Merely preloading the chunk (via <link rel="modulepreload">) does not execute shiki;
    // only rendering a CodeBlock fence imports and runs it.
    const violations: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        if (!msg.text().includes("style-src")) violations.push(msg.text());
      }
    });

    // Silence background polls.
    await page.route("**/cp.v1.SpawnService/ListApps", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: '{"apps":[]}' })
    );
    await page.route("**/cp.v1.SpawnService/ListSpawns", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          spawns: [{ spawnId: "test-csp-hl", name: "CSP Highlight", appId: "test", status: "SPAWN_STATUS_ACTIVE", mode: "acp", model: "test", modelApplied: true }],
        }),
      })
    );
    // Return an ACP session so SpawnTabs mounts AcpSessionPanel (not TerminalView).
    await page.route("**/cp.v1.SpawnService/ListSessions", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          sessions: [{ sessionId: "hl-1", transport: "SESSION_TRANSPORT_ACP", runnable: "agent", status: "active", pinned: true }],
        }),
      })
    );

    // Intercept the ACP WebSocket and inject a fenced code block frame.
    // When AcpSessionPanel connects and sends its bind frame, reply with an agent message
    // containing a Python code fence. This forces Streamdown to mount HighlightedCodeBlockBody
    // (the lazy shiki chunk) and execute it under the enforced CSP, catching any eval/wasm
    // regressions before they reach production.
    await page.routeWebSocket("**/ws/session", (ws) => {
      ws.onMessage(() => {
        ws.send(JSON.stringify({ kind: "agent", seq: 1, text: "Result:\n\n```python\nprint(\"hello\")\n```\n" }) + "\n");
        ws.send(JSON.stringify({ kind: "turn", seq: 2, state: "idle", queued: 0 }) + "\n");
      });
    });

    await page.goto("/spawn/test-csp-hl");
    // [data-language] is set by Streamdown's CodeBlock container — its presence proves the
    // fenced code fence rendered and HighlightedCodeBlockBody (the lazy shiki module) mounted.
    await page.waitForSelector("[data-language]", { timeout: 10000 });
    // Allow async highlighting (shiki updates state via useEffect) to settle.
    await page.waitForLoadState("networkidle");

    expect(violations, `Unexpected CSP violations:\n${violations.join("\n")}`).toHaveLength(0);
  });

  test("no inline scripts in HTML (theme-bootstrap externalized) [WM19]", async ({ page }) => {
    // The theme-bootstrap IIFE must be in an external hashed asset, not inline.
    const response = await page.goto("/");
    const html = await response!.text();

    // There should be no bare <script>...</script> tags (inline scripts) in the HTML.
    // External <script src="..."> tags are allowed (and must have integrity attributes).
    const inlineScriptRe = /<script(?![^>]*\ssrc=)[^>]*>[^<\s][^<]*<\/script>/;
    expect(html).not.toMatch(inlineScriptRe);
  });

  test("all script tags have integrity attributes (SRI) [WL4]", async ({ page }) => {
    const response = await page.goto("/");
    const html = await response!.text();

    // Every <script src="..."> must have integrity= and crossorigin= attributes,
    // AND the integrity value must match the actual on-disk bytes. A stale/wrong
    // hash means the browser rejects the script and the app never boots.
    const scriptTagRe = /<script[^>]+\bsrc="([^"]+)"[^>]*>/g;
    let m: RegExpExecArray | null;
    const missing: string[] = [];
    const mismatch: string[] = [];

    while ((m = scriptTagRe.exec(html)) !== null) {
      const tag = m[0];
      const src = m[1];
      if (!tag.includes("integrity=")) {
        missing.push(src);
        continue;
      }
      const integrityM = tag.match(/\bintegrity="([^"]+)"/);
      if (integrityM) {
        const claimed = integrityM[1];
        const fileName = src.replace(/^\//, "");
        const filePath = path.join(DIST_DIR, fileName);
        if (fs.existsSync(filePath)) {
          const actual = sha384FromDisk(filePath);
          if (actual !== claimed) {
            mismatch.push(`${src}: stamped integrity does not match disk bytes (browser would reject)`);
          }
        }
      }
    }

    expect(missing, `Script tags missing integrity:\n${missing.join("\n")}`).toHaveLength(0);
    expect(mismatch, `SRI integrity mismatch:\n${mismatch.join("\n")}`).toHaveLength(0);
  });

  test("all stylesheet links have integrity attributes (SRI) [WL4]", async ({ page }) => {
    const response = await page.goto("/");
    const html = await response!.text();

    // Same as the script check: verify integrity= is present AND matches on-disk bytes.
    const linkRe = /<link[^>]+\brel="stylesheet"[^>]*>/g;
    let m: RegExpExecArray | null;
    const missing: string[] = [];
    const mismatch: string[] = [];

    while ((m = linkRe.exec(html)) !== null) {
      const tag = m[0];
      const hrefM = tag.match(/href="([^"]+)"/);
      const src = hrefM ? hrefM[1] : tag;
      if (!tag.includes("integrity=")) {
        missing.push(src);
        continue;
      }
      const integrityM = tag.match(/\bintegrity="([^"]+)"/);
      if (integrityM && hrefM) {
        const claimed = integrityM[1];
        const fileName = hrefM[1].replace(/^\//, "");
        const filePath = path.join(DIST_DIR, fileName);
        if (fs.existsSync(filePath)) {
          const actual = sha384FromDisk(filePath);
          if (actual !== claimed) {
            mismatch.push(`${hrefM[1]}: stamped integrity does not match disk bytes (browser would reject)`);
          }
        }
      }
    }

    expect(missing, `Stylesheet links missing integrity:\n${missing.join("\n")}`).toHaveLength(0);
    expect(mismatch, `SRI integrity mismatch:\n${mismatch.join("\n")}`).toHaveLength(0);
  });

  test("sonner toast renders without CSP violations [WM19]", async ({ page }) => {
    // Exercises sonner's __insertCSS (called at module import time) and the <Toaster>
    // component rendering — the two code paths whose style injection necessitates
    // 'unsafe-inline' in style-src. If a regression adds eval/inline to script-src the
    // violation is caught here.
    const violations: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        if (!msg.text().includes("style-src")) violations.push(msg.text());
      }
    });

    // Route the publish API to return an error so the Publish form triggers toast.error().
    await page.route("**/cp.v1.SpawnService/RegisterAppVersion", (route) =>
      route.fulfill({ status: 500, contentType: "application/json", body: '{"code":"internal","message":"test error"}' })
    );
    // Silence background polls so we don't get noise in the console.
    await page.route("**/cp.v1.SpawnService/ListSpawns", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: '{"spawns":[]}' })
    );
    await page.route("**/cp.v1.SpawnService/ListApps", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: '{"apps":[]}' })
    );

    await page.goto("/publish");
    await page.waitForLoadState("networkidle");

    // AppShell always renders <Toaster>; verify the portal container is in the DOM.
    // This proves the sonner module loaded (triggering __insertCSS) and the component rendered.
    const toaster = page.locator("[data-sonner-toaster]");
    await expect(toaster).toBeAttached();

    // Trigger toast.error() via the Publish form's error path.
    await page.fill("[data-testid='publish-id']", "test/app");
    await page.fill("[data-testid='publish-title']", "Test");
    await page.fill("[data-testid='publish-version']", "1.0.0");
    await page.fill("[data-testid='publish-ref']", "test/app@abc123");
    await page.click("[data-testid='publish-submit']");

    // Wait for the toast to appear (proves toast.error() rendered under enforced CSP).
    await expect(page.locator("[data-sonner-toaster] li")).toBeAttached({ timeout: 5000 });

    expect(violations, `Unexpected CSP violations:\n${violations.join("\n")}`).toHaveLength(0);
  });

  test("xterm terminal renders without CSP violations (terminal render) [WM19]", async ({ page }) => {
    // Exercises xterm's terminal.open() style injection — the code path whose runtime
    // <style> injection necessitates 'unsafe-inline' in style-src [WM19]. Without this
    // test a dep regression that adds eval to xterm's rendering path would go undetected.
    const violations: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        if (!msg.text().includes("style-src")) violations.push(msg.text());
      }
    });

    // Mock the spawn list so the app binds the test spawn as active.
    await page.route("**/cp.v1.SpawnService/ListSpawns", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          spawns: [{ spawnId: "test-csp-spawn", name: "CSP Test", appId: "test", status: "SPAWN_STATUS_ACTIVE", mode: "tmux", model: "test", modelApplied: true }],
        }),
      })
    );

    // Return a mosh (tmux) session so SpawnTabs mounts TerminalView (not AcpSessionPanel).
    // xterm's terminal.open() fires when TerminalView's useEffect runs — before the WS dial.
    await page.route("**/cp.v1.SpawnService/ListSessions", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          sessions: [{ sessionId: "1", transport: "SESSION_TRANSPORT_MOSH", runnable: "bash", status: "active", pinned: true }],
        }),
      })
    );

    await page.goto("/spawn/test-csp-spawn");
    // Wait for TerminalView to mount and xterm.open() to fire.
    await expect(page.locator("[data-testid='terminal-view']")).toBeAttached({ timeout: 8000 });

    expect(violations, `Unexpected CSP violations:\n${violations.join("\n")}`).toHaveLength(0);
  });
});
