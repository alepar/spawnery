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
  // Headless Chromium running alongside Docker container churn (the per-test spawn create/teardown
  // flaps host veth interfaces) occasionally aborts a page.goto with net::ERR_NETWORK_CHANGED. That's
  // an environment flake, not an app bug — retry it so the suite is deterministic.
  retries: 2,
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
