/**
 * Playwright globalSetup: runs once, before any worker starts. Fail-fast gate — a broken target
 * or contract skew should never manifest as a confusing pile of per-test failures.
 *
 * Order: load config -> version pin -> preflight health -> generate/export the run id -> pre-run
 * stale-namespace sweep. The run id is exported via process.env so every worker process (forked
 * after this completes) shares the same namespace (design §Isolation, namespacing).
 */

import { loadTargetConfig } from "../config/target";
import { assertVersionPin } from "../config/version";
import { runPreflight } from "../fixtures/preflight";
import { preRunSweep } from "../fixtures/sweep";
import { newRunId } from "../fixtures/namespace";
import { AcceptanceClient } from "../drivers/oracle";
import { DevTokenAuth } from "../auth/devtoken";
import { OAuthPoPAuth } from "../auth/oauthpop";
import type { AuthStrategy } from "../auth/types";

export default async function globalSetup(): Promise<void> {
  const cfg = loadTargetConfig();

  assertVersionPin(cfg.targetRef, cfg.buildRef);

  if (cfg.identityPool.length === 0) {
    throw new Error("ACC_IDENTITY_POOL is empty — global setup needs at least one identity for preflight/sweep");
  }
  // Preflight/sweep are owner-agnostic (ListApps/ListSpawns scope by the caller's own owner), so
  // any pool identity works; the first one is used as this run's "system" credential. Mirror
  // test.ts's selectAuth so the global gate works in whichever auth mode the scenarios use.
  const auth: AuthStrategy =
    cfg.authMode === "dev-token"
      ? new DevTokenAuth()
      : new OAuthPoPAuth({ asOrigin: cfg.asOrigin, webOrigin: cfg.webOrigin });
  const systemIdentity = cfg.identityPool[0];
  const api = new AcceptanceClient({
    baseUrl: cfg.cpEndpoint,
    bearer: await auth.cpAccessToken(systemIdentity),
  });

  await runPreflight(cfg, api);

  const runId = cfg.runId ?? newRunId();
  process.env.ACC_RUN_ID = runId;
  console.log(`[acceptance] run id: ${runId}`);

  await preRunSweep(api, cfg.staleTtlMs);
}
