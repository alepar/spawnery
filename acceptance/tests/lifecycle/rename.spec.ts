/**
 * rename: web is a real, green flow (DOM primary + api oracle cross-check). spawnctl has no
 * rename verb, so the cli row is an intentional fixme that records product debt (see epic
 * sp-tq0t). CliDriver's executable unit test retains the fail-loud parityGap behavior.
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

test("rename · cli — PARITY GAP (intentional fixme; spawnctl has no rename — see epic sp-tq0t)", async ({
  cli,
  api,
  identity,
  ns,
  spawns,
}) => {
  test.fixme(true, "spawnctl has no rename command; the web rename flow remains covered above.");
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id);
  // Kept executable beneath test.fixme so implementing the verb turns this row into real coverage.
  await cli.rename(ctx, id, ns("renamed"));
});
