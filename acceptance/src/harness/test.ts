/**
 * The Playwright `test` extended with this suite's fixtures: per-worker identity/target/auth,
 * the apiDriver oracle, both surface drivers, run-namespacing, the prod-safety guardrail
 * (auto-run before every test), and an auto worker-teardown sweep. Scenarios (Phase 1+) import
 * `test`/`expect` from here instead of from `@playwright/test` directly.
 */

import { test as base, expect as baseExpect } from "@playwright/test";
import { loadTargetConfig, type TargetConfig } from "../config/target";
import { identityForWorker, type Identity } from "../fixtures/identity-pool";
import { nsName } from "../fixtures/namespace";
import { teardownSweep } from "../fixtures/sweep";
import { guardMutation } from "./guardrail";
import { ApiDriver, type SpawnSummary } from "../drivers/api";
import { WebDriver } from "../drivers/web";
import { CliDriver } from "../drivers/cli";
import { DevTokenAuth } from "../auth/devtoken";
import type { AuthStrategy } from "../auth/types";
import type { DriverCtx, SpawnStatus } from "../drivers/types";

interface WorkerFixtures {
  target: TargetConfig;
  runId: string;
  identity: Identity;
  auth: AuthStrategy;
  api: ApiDriver;
  web: WebDriver;
  cli: CliDriver;
  /** teardownSweeper is an auto, worker-scoped fixture: no test consumes it directly. */
  teardownSweeper: void;
}

interface TestFixtures {
  ns: (base: string) => string;
  ctx: DriverCtx;
  /** guardrail is an auto, test-scoped fixture: no test consumes it directly. */
  guardrail: void;
}

function selectAuth(mode: TargetConfig["authMode"]): AuthStrategy {
  if (mode === "dev-token") return new DevTokenAuth();
  // OAuthPoPAuth lands in sp-tq0t.3; fail loudly rather than silently falling back to dev-token.
  throw new Error(`auth mode ${JSON.stringify(mode)} is not implemented yet (see sp-tq0t.3)`);
}

export const test = base.extend<TestFixtures, WorkerFixtures>({
  target: [
    // eslint-disable-next-line no-empty-pattern -- Playwright fixture signature requires the first param
    async ({}, use) => {
      await use(loadTargetConfig());
    },
    { scope: "worker" },
  ],

  runId: [
    async ({ target }, use) => {
      // global-setup.ts generates one (if ACC_RUN_ID wasn't preset) and exports it via
      // process.env so every worker process shares the same run namespace.
      if (!target.runId) {
        throw new Error("ACC_RUN_ID is unset — global-setup.ts should have generated and exported one");
      }
      await use(target.runId);
    },
    { scope: "worker" },
  ],

  identity: [
    async ({ target }, use, workerInfo) => {
      await use(identityForWorker(target.identityPool, workerInfo.parallelIndex));
    },
    { scope: "worker" },
  ],

  auth: [
    async ({ target }, use) => {
      await use(selectAuth(target.authMode));
    },
    { scope: "worker" },
  ],

  api: [
    async ({ target, auth, identity }, use) => {
      await use(new ApiDriver(target.cpEndpoint, auth.oracleToken(identity)));
    },
    { scope: "worker" },
  ],

  web: [
    // eslint-disable-next-line no-empty-pattern -- Playwright fixture signature requires the first param
    async ({}, use) => {
      await use(new WebDriver());
    },
    { scope: "worker" },
  ],

  cli: [
    async ({ target }, use) => {
      await use(new CliDriver({ cpEndpoint: target.cpEndpoint, spawnctlBin: target.spawnctlBin }));
    },
    { scope: "worker" },
  ],

  // Worker-scoped teardown: sweeps this run's namespace once per worker, after its last test —
  // layer 1 of the two-layer cleanup (layer 2 is global-setup's pre-run sweep, which catches runs
  // whose process died before this ever ran). Auto so it always runs regardless of which
  // fixtures a given test actually used.
  teardownSweeper: [
    async ({ api, runId }, use) => {
      await use();
      await teardownSweep(api, runId);
    },
    { scope: "worker", auto: true },
  ],

  // Seed the dev-token localStorage override BEFORE the SPA boots (auth.seedWeb uses
  // page.addInitScript), by wrapping Playwright's own `page` fixture.
  page: async ({ page, auth, identity }, use) => {
    await auth.seedWeb(page, identity);
    await use(page);
  },

  ns: async ({ runId }, use, testInfo) => {
    await use((base: string) => nsName(runId, testInfo.workerIndex, base));
  },

  ctx: async ({ identity, ns, api, page }, use) => {
    await use({ identity, ns, api, page });
  },

  // Default-deny prod-safety guardrail: runs before every test's body via an auto fixture, using
  // Playwright's own tag mechanism (test(..., { tag: '@readonly' }, fn)) — untagged/@mutating
  // tests hard-fail against a host that isn't on the non-prod allowlist.
  guardrail: [
    async ({ target }, use, testInfo) => {
      guardMutation(testInfo.tags, target.targetHost, target.nonprodHosts);
      await use();
    },
    { auto: true },
  ],
});

export const expect = baseExpect.extend({
  toContainSpawn(received: SpawnSummary[], spawnId: string, opts: { status?: SpawnStatus } = {}) {
    const found = received.find((s) => s.spawnId === spawnId);
    const pass = !!found && (opts.status === undefined || found.status === opts.status);
    const wantSuffix = opts.status ? ` with status ${opts.status}` : "";
    return {
      pass,
      message: () =>
        pass
          ? `expected spawns not to contain ${spawnId}${wantSuffix}`
          : `expected spawns to contain ${spawnId}${wantSuffix}; found: ${
              received.map((s) => `${s.spawnId}=${s.status}`).join(", ") || "(none)"
            }`,
    };
  },
});

declare module "@playwright/test" {
  interface Matchers<R, _T> {
    toContainSpawn(spawnId: string, opts?: { status?: SpawnStatus }): R;
  }
}
