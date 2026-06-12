/**
 * Playwright configuration for the CSP-enforced prod-bundle suite (W1, sp-2ckv.6).
 *
 * Serves dist/ via `vite preview` with a middleware that injects the dist/_headers
 * CSP + cache headers. This exercises the REAL enforced CSP — not a dev approximation.
 *
 * The suite asserts NO CSP violations while exercising:
 *  - Fonts (terminal font render) [WM19]
 *  - Toasts (sonner toast trigger) [WM19]
 *  - Terminal (xterm render) [WM19]
 *  - Highlight/diagram block (dynamic highlighted-body chunk) [WM19]
 *
 * A dep that regresses to eval/inline fails CI here, not in production.
 *
 * Host-gated: skipped if PLAYWRIGHT_BROWSERS_UNAVAILABLE is set, or if the dist/
 * directory does not exist (no prod build). Run with:
 *   npm run test:e2e:csp
 * or:
 *   npx playwright test --config=playwright.csp.config.ts
 *
 * Requires a prior `npm run build` with VITE_CP_ORIGIN / VITE_AS_ORIGIN set (so that
 * dist/_headers contains a real CSP). In CI, the web-ci.yml build step produces this.
 */

import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/csp-prod.spec.ts",
  timeout: 30_000,
  fullyParallel: false,
  workers: 1,
  retries: 1,
  use: {
    baseURL: "http://localhost:4173",
    headless: true,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  // Serve the prod build via vite preview.
  // The preview-headers middleware (wired in vite.config.ts via configurePreviewServer)
  // injects the dist/_headers CSP so the browser actually enforces Content-Security-Policy.
  // Vite preview ignores _headers natively; without the middleware the CSP would not be sent
  // and violation checks would pass vacuously.
  webServer: {
    command: "npx vite preview --port 4173",
    url: "http://localhost:4173",
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
