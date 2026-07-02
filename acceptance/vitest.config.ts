import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    // src/ holds the pure driver/fixture units; tests/<phase>/*.test.ts holds each phase's
    // scenario-support units (e.g. tests/sessions/support.test.ts) — both are hermetic vitest,
    // never the live Playwright specs (those match tests/**/*.spec.ts, run via test:accept).
    include: ["src/**/*.test.ts", "tests/**/*.test.ts"],
  },
});
