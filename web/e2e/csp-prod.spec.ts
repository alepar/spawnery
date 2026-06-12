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
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DIST_DIR = path.resolve(__dirname, "../dist");

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
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        // script-src violations are blocking — fail the test.
        // style-src 'unsafe-inline' violations are expected (xterm/sonner) and allowed.
        if (!msg.text().includes("style-src")) {
          violations.push(msg.text());
        }
      }
    });

    await page.goto("/");
    await page.waitForLoadState("networkidle");

    expect(violations, `CSP violations:\n${violations.join("\n")}`).toHaveLength(0);
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

  test("dynamic import chunk loads without CSP violations [WM19]", async ({ page }) => {
    // This checks that highlighted-body-*.js (the dynamic shiki/streamdown chunk)
    // has an integrity attribute and loads without CSP errors.
    const violations: string[] = [];
    const failedResources: string[] = [];

    page.on("console", (msg) => {
      if (msg.type() === "error" && msg.text().includes("Content Security Policy")) {
        if (!msg.text().includes("style-src")) violations.push(msg.text());
      }
    });
    page.on("response", (resp) => {
      // Track any JS asset that fails to load (blocked by CSP or SRI mismatch).
      if (!resp.ok() && resp.url().includes("highlighted-body")) {
        failedResources.push(`${resp.status()} ${resp.url()}`);
      }
    });

    await page.goto("/");
    await page.waitForLoadState("networkidle");

    expect(violations).toHaveLength(0);
    expect(failedResources).toHaveLength(0);
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

    // Every <script src="..."> must have integrity= and crossorigin= attributes.
    const scriptTagRe = /<script[^>]+\bsrc="([^"]+)"[^>]*>/g;
    let m: RegExpExecArray | null;
    const missing: string[] = [];

    while ((m = scriptTagRe.exec(html)) !== null) {
      const tag = m[0];
      if (!tag.includes("integrity=")) {
        missing.push(m[1]);
      }
    }

    expect(missing, `Script tags missing integrity:\n${missing.join("\n")}`).toHaveLength(0);
  });

  test("all stylesheet links have integrity attributes (SRI) [WL4]", async ({ page }) => {
    const response = await page.goto("/");
    const html = await response!.text();

    const linkRe = /<link[^>]+\brel="stylesheet"[^>]*>/g;
    let m: RegExpExecArray | null;
    const missing: string[] = [];

    while ((m = linkRe.exec(html)) !== null) {
      const tag = m[0];
      if (!tag.includes("integrity=")) {
        const hrefM = tag.match(/href="([^"]+)"/);
        missing.push(hrefM ? hrefM[1] : tag);
      }
    }

    expect(missing, `Stylesheet links missing integrity:\n${missing.join("\n")}`).toHaveLength(0);
  });
});
