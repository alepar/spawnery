/**
 * Browse/search/detail — read-only, safe on any target including prod (no writes). Cross-checks the
 * rendered web DOM against the market oracle (design §Assertion strategy: web DOM is primary, the
 * oracle is a complementary cross-check).
 *
 * Needs the documented seed-app precondition (tests/marketplace/README.md, ACC_SEED_APP_ID) — a
 * real, listed app. If it's absent from BOTH the DOM and the oracle, that's an environment error
 * (fail loudly naming the precondition), not a scenario failure or a skip.
 */

import { test, expect, seedAppId } from "./market-fixtures";

test("browse renders the seed app @readonly", { tag: "@readonly" }, async ({ market, ctx }) => {
  const seed = seedAppId();
  const domRows = await market.web.browse(ctx);
  const oracleApps = await market.oracle.listApps();
  const inDom = domRows.some((r) => r.id === seed);
  const inOracle = oracleApps.some((a) => a.id === seed);

  if (!inDom && !inOracle) {
    throw new Error(
      `marketplace browse precondition unmet: seed app ${JSON.stringify(seed)} is not in Browse or ListApps on ` +
        `this target — register it first (see tests/marketplace/README.md's seeding recipe) or point ` +
        `ACC_SEED_APP_ID at an existing listed app`,
    );
  }
  expect(inDom).toBe(true);
  expect(inOracle).toBe(true);
});

test("search filters @readonly", { tag: "@readonly" }, async ({ market, ctx }) => {
  const seed = seedAppId();
  // ListApps/Catalog filters on display_name/summary/tags (internal/cp/store/apps.go's Catalog) —
  // NOT on the app id — so the search token must come from the seed's displayName, not its id.
  const seedApp = await market.oracle.getApp(seed);
  const token = (seedApp.app.displayName || seed.split("/").pop() || seed).split(/\s+/)[0].toLowerCase();

  const filtered = await market.web.browse(ctx, token);
  expect(filtered.some((r) => r.id === seed)).toBe(true);

  const empty = await market.web.browse(ctx, "acc-nonsense-query-that-matches-nothing-zzz");
  expect(empty).toEqual([]);
});

test("detail shows versions @readonly", { tag: "@readonly" }, async ({ market, ctx }) => {
  const seed = seedAppId();
  const detail = await market.web.openDetail(ctx, seed);
  expect(detail.versions.length).toBeGreaterThan(0);

  const oracleDetail = await market.oracle.getApp(seed);
  expect(oracleDetail.versions.length).toBeGreaterThan(0);
  expect([...detail.versions].sort()).toEqual(oracleDetail.versions.map((v) => v.version).sort());
});
