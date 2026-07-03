/**
 * market-fixtures.ts: extends the Phase-0 harness `test` (src/harness/test.ts) with a worker-scoped
 * `market` fixture bundling the marketplace drivers ({web,cli,oracle}), plus an auto worker-teardown
 * that unlists this identity's `acc-*` apps (best-effort — there is no DeleteApp RPC, see
 * tests/marketplace/README.md).
 *
 * The `market` fixture reads ACC_SEED_APP_ID directly rather than extending TargetConfig, and lives
 * entirely under tests/marketplace/ — zero edits to the shared Phase-0 files, per the sp-tq0t.7 plan's
 * isolation note (keeps this task's file set disjoint from sibling phases).
 */

import { test as base, expect } from "../../src/harness/test";
import { CliMarketDriver, WebMarketDriver } from "../../src/drivers/market";
import { MarketOracle } from "../../src/drivers/market-oracle";
import { isAccArtifact } from "../../src/fixtures/namespace";
import type { DriverCtx } from "../../src/drivers/types";

export interface MarketFixture {
  web: WebMarketDriver;
  cli: CliMarketDriver;
  oracle: MarketOracle;
}

interface MarketWorkerFixtures {
  market: MarketFixture;
  /** marketTeardownSweeper is an auto, worker-scoped fixture: no test consumes it directly. */
  marketTeardownSweeper: void;
}

/**
 * marketAppId builds a namespaced, registerable app id.
 *
 * CORRECTION vs the sp-tq0t.7 plan: internal/cp/validate.go's validateManifest requires the id be
 * exactly "creator/app" (two lowercase [a-z0-9._-]+ segments) — a bare `ctx.ns(base)` value (no
 * slash) is rejected with InvalidArgument, so it cannot be used verbatim as the plan's grounding
 * facts assumed. The fixed "acc" creator segment plus ns()'s "acc-"-prefixed app segment satisfies
 * the regex while staying run+worker-unique and recognizable by `isAccAppId` below.
 */
export function marketAppId(ctx: DriverCtx, base: string): string {
  return `acc/${ctx.ns(base)}`;
}

/** isAccAppId reports whether a market app id was created by this suite (see marketAppId). */
export function isAccAppId(id: string): boolean {
  const parts = id.split("/");
  return parts.length === 2 && isAccArtifact(parts[1]);
}

/** seedAppId is the required, documented precondition: a real, listed, spawnable app (README.md). */
export function seedAppId(): string {
  return process.env.ACC_SEED_APP_ID ?? "spawnery/secret-app";
}

export const test = base.extend<Record<never, never>, MarketWorkerFixtures>({
  market: [
    async ({ target, auth, identity }, use) => {
      await use({
        web: new WebMarketDriver(),
        cli: new CliMarketDriver({ cpEndpoint: target.cpEndpoint, spawnctlBin: target.spawnctlBin }),
        oracle: new MarketOracle(target.cpEndpoint, await auth.oracleToken(identity)),
      });
    },
    { scope: "worker" },
  ],

  // Worker-scoped teardown: unlists every acc-* app this identity registered, even after a test
  // failure. Best-effort (log, never throw) — there is no DeleteApp RPC, so rows persist regardless
  // (see README.md's no-DeleteApp cleanup caveat); this only keeps Browse clean for other users.
  marketTeardownSweeper: [
    async ({ market }, use) => {
      await use();
      let mine: { id: string }[] = [];
      try {
        mine = await market.oracle.listMyApps();
      } catch (e) {
        console.warn(`marketTeardownSweeper: listMyApps failed: ${(e as Error).message}`);
        return;
      }
      for (const app of mine) {
        if (!isAccAppId(app.id)) continue;
        try {
          await market.oracle.setAppListing(app.id, false);
        } catch (e) {
          console.warn(`marketTeardownSweeper: failed to unlist ${app.id}: ${(e as Error).message}`);
        }
      }
    },
    { scope: "worker", auto: true },
  ],
});

export { expect };
