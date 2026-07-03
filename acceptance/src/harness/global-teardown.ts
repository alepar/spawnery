/**
 * Playwright globalTeardown: runs once, after all workers finish. Best-effort final sweep of the
 * whole run's namespace — redundant with each worker's own teardownSweeper (harness/test.ts) in
 * the common case, but catches anything a worker-scoped sweep missed (e.g. a worker process that
 * was killed before its own teardown fixture ran). Never throws: a cleanup failure here should
 * not turn a green run red, or mask the real teardown-sweeper failure of the worker it belonged to.
 */

import { loadTargetConfig } from "../config/target";
import { teardownSweep } from "../fixtures/sweep";
import { ApiDriver } from "../drivers/api";
import { DevTokenAuth } from "../auth/devtoken";

export default async function globalTeardown(): Promise<void> {
  const cfg = loadTargetConfig();
  if (cfg.authMode !== "dev-token" || cfg.identityPool.length === 0 || !cfg.runId) {
    // Mirrors global-setup's preconditions; if they didn't hold, global-setup already failed the
    // run before any test executed, so there is nothing of ours to sweep.
    return;
  }
  const auth = new DevTokenAuth();
  const api = new ApiDriver(cfg.cpEndpoint, await auth.oracleToken(cfg.identityPool[0]));
  try {
    await teardownSweep(api, cfg.runId);
  } catch (e) {
    console.warn(`[acceptance] global-teardown sweep failed (non-fatal): ${(e as Error).message}`);
  }
}
