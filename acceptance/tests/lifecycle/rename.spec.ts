/**
 * rename: web is a real, green flow (DOM primary + api oracle cross-check). spawnctl has no
 * rename verb — the cli row is a PARITY GAP that fails red by design (visible red over silent
 * skip; see epic sp-tq0t and cliDriver.rename's parityGap throw).
 */

import { test, expect, requireEnv, cliCtx } from "../../src/harness/scenario";

const appId = requireEnv("ACC_LIFECYCLE_APP");

test("rename · web", async ({ web, api, ctx, ns, spawns }) => {
  const id = spawns.track(await web.createSpawn(ctx, { appId }));
  await web.waitActive(ctx, id);
  const newName = ns("renamed"); // namespaced ⇒ also sweep-visible
  await web.rename(ctx, id, newName);
  // DOM primary: the row's rendered name updates.
  const listed = await web.list(ctx);
  expect(listed.find((s) => s.spawnId === id)?.name).toBe(newName);
  // Oracle cross-check.
  await expect.poll(async () => (await api.findSpawn(id))?.name, { timeout: 30_000 }).toBe(newName);
});

test("rename · cli — PARITY GAP (fails red by design; spawnctl has no rename — see epic sp-tq0t)", async ({
  cli,
  api,
  identity,
  ns,
  spawns,
}) => {
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id);
  // Recorded design decision: full coverage, let it fail. This throws parityGap("rename") → RED.
  await cli.rename(ctx, id, ns("renamed"));
});
