/**
 * Register (app-version) — dual-surface (web Publish form + `spawnctl -register`), cross-checked
 * via the market oracle and (web only) the rendered My Apps DOM. Mutating: hard-fails off the
 * non-prod allowlist (the guardrail's default-deny).
 */

import { test, expect, marketAppId } from "./market-fixtures";
import type { RegisterSpec } from "../../src/drivers/market";

for (const surface of ["web", "cli"] as const) {
  test(`register app version + appears in my-apps · ${surface} @mutating`, { tag: "@mutating" }, async ({ market, ctx }) => {
    const appId = marketAppId(ctx, `probe-${surface}`);
    const spec: RegisterSpec = {
      id: appId,
      title: `Acc Probe ${surface}`,
      tags: ["acc"],
      version: "1.0.0",
      ref: `${appId}@ci`,
    };

    const drv = surface === "web" ? market.web : market.cli;
    const result = await drv.register(ctx, spec);
    expect(result).toEqual({ appId, version: "1.0.0" });

    const detail = await market.oracle.getApp(appId);
    expect(detail.app.id).toBe(appId);
    expect(detail.app.latestVersion).toBe("1.0.0");
    expect(detail.versions.some((v) => v.version === "1.0.0" && v.tier === "TRUST_TIER_UNVERIFIED")).toBe(true);

    const mine = await market.oracle.listMyApps();
    expect(mine.some((a) => a.id === appId)).toBe(true);

    if (surface === "web") {
      const domMine = await market.web.listMine(ctx);
      expect(domMine.some((r) => r.id === appId)).toBe(true);
    }
  });
}
