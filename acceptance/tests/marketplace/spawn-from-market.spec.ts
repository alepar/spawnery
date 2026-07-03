/**
 * Spawn from a market listing — dual-surface (web Detail's "Spawn" button + `spawnctl -app-id`),
 * reusing the Phase-0 SpawnDriver/teardownSweeper (spawns ARE deletable, unlike apps). No agent:
 * this only asserts the spawn reaches ACTIVE, never drives the LLM (epic table, Phase 4 = agent:none).
 *
 * Needs the same seed-app precondition as browse.spec.ts (tests/marketplace/README.md,
 * ACC_SEED_APP_ID): a real, listed, SPAWNABLE app. A create failure is reported naming that
 * precondition rather than surfacing as an opaque driver error.
 */

import { test, expect, seedAppId } from "./market-fixtures";

for (const surface of ["web", "cli"] as const) {
  test(`spawn from a market listing · ${surface} @mutating`, { tag: "@mutating" }, async ({ api, ctx, web, cli }) => {
    const drv = surface === "web" ? web : cli;
    const seed = seedAppId();

    let id: string;
    try {
      id = await drv.createSpawn(ctx, { appId: seed });
    } catch (e) {
      throw new Error(
        `spawn-from-market precondition unmet: could not create a spawn from seed app ${JSON.stringify(seed)} — ` +
          `it must be a real, listed, SPAWNABLE app (see tests/marketplace/README.md, ACC_SEED_APP_ID). ` +
          `Original error: ${(e as Error).message}`,
      );
    }

    await drv.waitActive(ctx, id);

    const found = await api.findSpawn(id);
    expect(found).toBeTruthy();
    expect(found).toMatchObject({ status: "ACTIVE" });
  });
}
