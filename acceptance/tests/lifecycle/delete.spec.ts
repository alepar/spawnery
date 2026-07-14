/**
 * delete: neither surface can delete (web has no UI affordance; spawnctl has no delete verb), so
 * the green delete verification goes through the api oracle (already the teardown actuator),
 * asserting the spawn disappears from ListSpawns AND from the web-rendered list. The two surface
 * gaps are intentional fixme rows below, recording product debt without failing the suite (see
 * epic sp-tq0t). Driver unit tests retain fail-loud stubs for accidental direct use.
 */

import { test, expect, requireEnv, cliCtx } from "../../src/harness/scenario";

const appId = requireEnv("ACC_LIFECYCLE_APP");

test("delete · api oracle (reflected on web list)", async ({ api, web, ctx, ns, spawns }) => {
  const name = ns("del");
  const id = spawns.track(await api.createSpawn({ appId, name })); // api CAN name it
  await expect.poll(async () => (await api.findSpawn(id))?.status, { timeout: 60_000 }).toBe("ACTIVE");
  await api.deleteSpawn(id);
  // Oracle: gone from ListSpawns (or terminal DELETED).
  await expect
    .poll(
      async () => {
        const s = await api.findSpawn(id);
        return s === undefined || s.status === "DELETED";
      },
      { timeout: 30_000 },
    )
    .toBe(true);
  // Surface reflection: web-rendered list no longer shows an ACTIVE row for it.
  await expect
    .poll(async () => {
      const listed = await web.list(ctx);
      return listed.every((s) => s.spawnId !== id || s.status === "DELETED");
    }, { timeout: 30_000 })
    .toBe(true);
});

test("delete · web — PRODUCT GAP (intentional fixme; no SPA affordance — see epic sp-tq0t)", async ({
  web,
  ctx,
  spawns,
}) => {
  test.fixme(true, "The SPA has no DeleteSpawn affordance; API deletion remains covered above.");
  const id = spawns.track(await web.createSpawn(ctx, { appId }));
  await web.waitActive(ctx, id);
  await web.delete(ctx, id); // fail-loud driver behavior remains covered by its unit test
});

test("delete · cli — PARITY GAP (intentional fixme; spawnctl has no delete — see epic sp-tq0t)", async ({
  cli,
  api,
  identity,
  ns,
  spawns,
}) => {
  test.fixme(true, "spawnctl has no delete command; API deletion remains covered above.");
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id);
  await cli.delete(ctx, id); // fail-loud driver behavior remains covered by its unit test
});
