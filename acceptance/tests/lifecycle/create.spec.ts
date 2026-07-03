/**
 * create + list, dual-surface: create a spawn, wait for it to become ACTIVE, and confirm the
 * surface's own list view shows it — cross-checked against the api oracle.
 */

import { test, expect, requireEnv, cliCtx } from "../../src/harness/scenario";

const appId = requireEnv("ACC_LIFECYCLE_APP");

test("create + list · web", async ({ web, api, ctx, spawns }) => {
  const id = spawns.track(await web.createSpawn(ctx, { appId }));
  await web.waitActive(ctx, id); // asserts the active dot renders (DOM primary)
  const listed = await web.list(ctx);
  expect(listed.some((s) => s.spawnId === id && s.status === "ACTIVE")).toBe(true);
  expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" }); // oracle cross-check
});

test("create + list · cli", async ({ cli, api, identity, ns, spawns }) => {
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id); // polls `spawnctl status`
  const listed = await cli.list(ctx);
  expect(listed.some((s) => s.spawnId === id)).toBe(true);
  expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });
});
