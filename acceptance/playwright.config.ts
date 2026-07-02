import { defineConfig, devices } from "@playwright/test";

// Points at an ALREADY-RUNNING spawnery instance (ACC_WEB_ORIGIN) — this suite provisions
// nothing. Load your target's .env.<target> before running (see README.md / .env.example).
//
// @agent describe-blocks (added in later phases) MUST configure retries: 0 — a real-LLM-agent
// failure never auto-retries (masks regressions and multiplies OpenRouter cost, design §NFRs):
//   test.describe("some @agent flow", () => {
//     test.describe.configure({ retries: 0 });
//     ...
//   });
export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  retries: 2, // infra flake only (design §Risks) — NOT for @agent scenarios, see note above
  workers: process.env.ACC_WORKERS ? Number(process.env.ACC_WORKERS) : undefined,
  reporter: [["html", { open: "never" }], ["list"]],
  globalSetup: "./src/harness/global-setup.ts",
  globalTeardown: "./src/harness/global-teardown.ts",
  use: {
    baseURL: process.env.ACC_WEB_ORIGIN,
    trace: "retain-on-failure",
    // Trace/video/HAR/state may carry auth material; redacted before upload — see
    // fixtures/redact.ts and the CI workflow's artifact-upload step.
    video: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
