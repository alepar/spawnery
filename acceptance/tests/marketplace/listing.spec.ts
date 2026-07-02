/**
 * Listing toggle (web My Apps) drives Browse visibility: a fresh registration is listed by default
 * (RegisterAppVersion upserts Listed:true, internal/cp/registration.go); unlisting removes it from
 * Browse/ListApps but the owner's ListMyApps still shows it (management view, includes unlisted).
 */

import { test, expect, marketAppId } from "./market-fixtures";
import type { RegisterSpec } from "../../src/drivers/market";

test("listing toggle drives Browse visibility @mutating", { tag: "@mutating" }, async ({ market, ctx }) => {
  const appId = marketAppId(ctx, "listing");
  const spec: RegisterSpec = {
    id: appId,
    title: "Acc Listing Probe",
    tags: ["acc"],
    version: "1.0.0",
    ref: `${appId}@ci`,
  };
  await market.web.register(ctx, spec);

  const listedInOracle = await market.oracle.listApps();
  expect(listedInOracle.some((a) => a.id === appId)).toBe(true);
  const listedInDom = await market.web.browse(ctx);
  expect(listedInDom.some((r) => r.id === appId)).toBe(true);

  await market.web.setListing(ctx, appId, false);

  const unlistedInOracle = await market.oracle.listApps();
  expect(unlistedInOracle.some((a) => a.id === appId)).toBe(false);
  const unlistedInDom = await market.web.browse(ctx);
  expect(unlistedInDom.some((r) => r.id === appId)).toBe(false);

  // Unlisted rows stay in the owner's management view — never asserted as "deleted" (no DeleteApp RPC).
  const mine = await market.oracle.listMyApps();
  expect(mine.some((a) => a.id === appId)).toBe(true);
});
