/**
 * Reusable scenario harness for Phase 1+ lifecycle scenarios: the Phase-0 `test` extended with a
 * `spawns` fixture (per-test SpawnRegistry cleanup) plus small env/ctx helpers. Scenario spec
 * files import `test`/`expect` from here instead of from `./test` directly.
 */

import { test as base, expect } from "./test";
import { SpawnRegistry } from "./spawn-registry";
import type { DriverCtx } from "../drivers/types";
import type { ApiDriver } from "../drivers/api";
import type { Identity } from "../fixtures/identity-pool";

interface ScenarioFixtures {
  /** spawns: per-test cleanup registry — track every created id so nothing leaks. */
  spawns: SpawnRegistry;
}

export const test = base.extend<ScenarioFixtures>({
  spawns: async ({ api }, use) => {
    const reg = new SpawnRegistry(api);
    await use(reg);
    await reg.cleanup();
  },
});

export { expect };

/** requireEnv reads a mandatory scenario env var, failing loudly (fail, never skip). */
export function requireEnv(name: string, env: NodeJS.ProcessEnv = process.env): string {
  const v = env[name];
  if (!v) throw new Error(`missing required env var ${name} (see acceptance/.env.example) — set it to run lifecycle scenarios`);
  return v;
}

/** cliCtx builds a page-less DriverCtx for cli scenarios (avoids launching a browser). */
export function cliCtx(parts: { identity: Identity; ns: (b: string) => string; api: ApiDriver }): DriverCtx {
  return { identity: parts.identity, ns: parts.ns, api: parts.api };
}
