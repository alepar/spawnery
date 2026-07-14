/**
 * The Playwright `test` extended with this suite's fixtures: per-worker identity/target/auth,
 * the apiDriver oracle, both surface drivers, run-namespacing, the prod-safety guardrail
 * (auto-run before every test), and an auto worker-teardown sweep. Scenarios (Phase 1+) import
 * `test`/`expect` from here instead of from `@playwright/test` directly.
 */

import { test as base, expect as baseExpect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { loadTargetConfig, type TargetConfig } from "../config/target";
import { identityForWorker, type Identity } from "../fixtures/identity-pool";
import { nsName } from "../fixtures/namespace";
import { teardownSweep } from "../fixtures/sweep";
import { CostLedger } from "../fixtures/budget";
import { guardMutation } from "./guardrail";
import { cleanupNewSpawns } from "./spawn-registry";
import { AcceptanceClient, createKnownVMTargetVerifier, type SpawnSummary } from "../drivers/oracle";
import { WebDriver } from "../drivers/web";
import { CliDriver } from "../drivers/cli";
import { DevTokenAuth } from "../auth/devtoken";
import { OAuthPoPAuth } from "../auth/oauthpop";
import type { AuthStrategy } from "../auth/types";
import type { DriverCtx, SpawnStatus } from "../drivers/types";

interface WorkerFixtures {
  target: TargetConfig;
  runId: string;
  identity: Identity;
  auth: AuthStrategy;
  api: AcceptanceClient;
  web: WebDriver;
  /** ledger: per-worker CostLedger for @agent scenarios to feed usage into + check() against. */
  ledger: CostLedger;
  cliConfigHome: string;
  /** teardownSweeper is an auto, worker-scoped fixture: no test consumes it directly. */
  teardownSweeper: void;
}

interface TestFixtures {
  ns: (base: string) => string;
  ctx: DriverCtx;
  cli: CliDriver;
  /** guardrail is an auto, test-scoped fixture: no test consumes it directly. */
  guardrail: void;
  /** postTestSpawnCleanup removes only owner-visible spawns created by this test. */
  postTestSpawnCleanup: void;
}

function selectAuth(target: TargetConfig): AuthStrategy {
  if (target.authMode === "dev-token") return new DevTokenAuth();
  const verifyTarget = createKnownVMTargetVerifier({
    rootCAPEM: readFileSync(target.rootCAPath!, "utf8"),
    trustDomain: target.trustDomain!,
    expectedNodeId: "node-1",
    expectedNodeClass: "cloud",
    expectedNodeAccountId: target.cloudAccountId!,
  });
  return new OAuthPoPAuth({ asOrigin: target.asOrigin, webOrigin: target.webOrigin, verifyTarget });
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
      await use(selectAuth(target));
    },
    { scope: "worker" },
  ],

  api: [
    // A token-provider function, not a resolved string: OAuthPoPAuth's bearer expires (15 min)
    // and must be re-fetched (proactively refreshed) on a run that outlives that TTL — see
    // drivers/oracle.ts's TokenSource and auth/oauthpop.ts. keyStore is the identity's signing
    // key (fresh for dev-token; the cnf-bound session key for OAuth-PoP — see auth/types.ts).
    async ({ target, auth, identity }, use) => {
      const keyStore = await auth.sessionKeyStore(identity);
      await use(new AcceptanceClient({
        baseUrl: target.cpEndpoint,
        bearer: () => auth.cpAccessToken(identity),
        keyStore,
        getNodeAccessToken: () => auth.nodeAccessToken(identity),
        verifyTarget: auth.targetVerifier(identity),
      }));
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

  cliConfigHome: [
    async ({ runId }, use, workerInfo) => {
      const dir = await mkdtemp(join(tmpdir(), `spawnery-acc-${runId}-${workerInfo.parallelIndex}-`));
      try { await use(dir); } finally { await rm(dir, { recursive: true, force: true }); }
    },
    { scope: "worker" },
  ],

  cli: async ({ target, auth, identity, page, cliConfigHome }, use) => {
    const prepared = await auth.prepareCli(page, identity, {
      spawnctlBin: target.spawnctlBin,
      asOrigin: target.asOrigin,
      configHome: cliConfigHome,
    });
    await use(new CliDriver({
      cpEndpoint: target.cpEndpoint,
      spawnctlBin: target.spawnctlBin,
      authArgs: prepared.authArgs,
      configHome: prepared.configHome,
      trust: target.rootCAPath ? {
        rootCAPath: target.rootCAPath,
        trustDomain: target.trustDomain!,
        crlStatePath: target.crlStatePath!,
        crlIssuerPaths: target.crlIssuerPaths ?? [],
        crlPaths: target.crlPaths ?? [],
      } : undefined,
    }));
  },

  ledger: [
    async ({ target }, use) => {
      await use(new CostLedger(target.tokenBudget, target.wallclockMs));
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

  // Prime auth BEFORE the test body runs, by wrapping Playwright's own `page` fixture:
  // DevTokenAuth seeds a localStorage override via page.addInitScript; OAuthPoPAuth drives the
  // real login button + fake-IdP round-trip (auth/oauthpop.ts).
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

  // Snapshot by opaque id, not by name: web/CLI-created spawns use product-default names, and a
  // failed CLI create can leave an ERROR row without returning its id. The baseline prevents this
  // cleanup from touching any owner resource that existed before the test.
  postTestSpawnCleanup: [
    async ({ api }, use) => {
      const baseline = new Set((await api.listSpawns()).map((spawn) => spawn.spawnId));
      try {
        await use();
      } finally {
        try {
          await cleanupNewSpawns(api, baseline);
        } catch (e) {
          console.warn(`postTestSpawnCleanup: failed to list owner spawns: ${(e as Error).message}`);
        }
      }
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
