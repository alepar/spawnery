/**
 * set-model, dual-surface: create a spawn, drive the surface's set-model action, and cross-check
 * the api oracle until the change is both recorded and applied (model application may be async).
 */

import { test, expect, requireEnv, cliCtx } from "../../src/harness/scenario";

const appId = requireEnv("ACC_LIFECYCLE_APP");
const model = requireEnv("ACC_TEST_MODEL");

test("set-model · web", async ({ web, api, ctx, spawns }) => {
  const id = spawns.track(await web.createSpawn(ctx, { appId }));
  await web.waitActive(ctx, id);
  await web.setModel(ctx, id, model);
  await expect
    .poll(
      async () => {
        const s = await api.findSpawn(id);
        return s ? { model: s.model, applied: s.modelApplied } : null;
      },
      { timeout: 60_000 },
    )
    .toEqual({ model, applied: true });
});

test("set-model · cli", async ({ cli, api, identity, ns, spawns }) => {
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id);
  await cli.setModel(ctx, id, model);
  await expect
    .poll(
      async () => {
        const s = await api.findSpawn(id);
        return s ? { model: s.model, applied: s.modelApplied } : null;
      },
      { timeout: 60_000 },
    )
    .toEqual({ model, applied: true });
});
