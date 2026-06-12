/**
 * Playwright configuration for auth e2e tests: login via A1's fake GitHub provider
 * + cross-origin /refresh cold-reload [AM2].
 *
 * HOST-GATED: run with E2E_AUTH=1. Tests skip themselves when the env var is absent,
 * so this config is safe to include in CI pipelines that lack a live AS.
 *
 * Starts a separate Vite dev server on port 5174 with VITE_AUTH_ENABLED=1 so that
 * authEnabled() returns true and the SPA goes through the real login flow instead of
 * the dev-token bypass. The existing Vite proxy rules (/oauth, /refresh, /logout)
 * default to 127.0.0.1:8090, where global-setup-auth.ts starts the AS.
 *
 * Run:
 *   E2E_AUTH=1 npx playwright test --config=playwright.auth.config.ts
 */
import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/auth.spec.ts",
  globalSetup: "./e2e/global-setup-auth.ts",
  globalTeardown: "./e2e/global-teardown-auth.ts",
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  retries: 1,
  use: {
    baseURL: "http://localhost:5174",
    headless: true,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    // VITE_AUTH_ENABLED=1: makes authEnabled() return true in the SPA without needing
    // VITE_AS_ORIGIN (which would cause the proxy to loop back to itself). AS calls use
    // relative paths, proxied to 127.0.0.1:8090 by Vite's existing /oauth,/refresh rules.
    command: "VITE_AUTH_ENABLED=1 npm run dev -- --port 5174",
    url: "http://localhost:5174",
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
