/**
 * stop: web is a real, green flow (DOM primary + api oracle cross-check on status). spawnctl has
 * no stop verb, so the cli row is an intentional fixme that records product debt (see epic
 * sp-tq0t). CliDriver's executable unit test retains the fail-loud parityGap behavior.
 */

import { test, expect, requireEnv, cliCtx } from "../../src/harness/scenario";

const appId = requireEnv("ACC_LIFECYCLE_APP");

test("stop · web", async ({ web, api, ctx, spawns }) => {
  const id = spawns.track(await web.createSpawn(ctx, { appId }));
  await web.waitActive(ctx, id);
  await web.stop(ctx, id);
  // Oracle cross-check: status transitions off ACTIVE. Accept any of the target's stopped
  // terminals (SUSPENDED/DELETED/ERROR/...) — assert !== ACTIVE/STARTING to avoid over-pinning
  // the exact post-stop status.
  await expect
    .poll(
      async () => {
        const status = (await api.findSpawn(id))?.status;
        return status !== "ACTIVE" && status !== "STARTING";
      },
      { timeout: 60_000 },
    )
    .toBe(true);
});

test("stop · cli — PARITY GAP (intentional fixme; spawnctl has no stop — see epic sp-tq0t)", async ({
  cli,
  api,
  identity,
  ns,
  spawns,
}) => {
  test.fixme(true, "spawnctl has no stop command; the web stop flow remains covered above.");
  const ctx = cliCtx({ identity, ns, api });
  const id = spawns.track(await cli.createSpawn(ctx, { appId }));
  await cli.waitActive(ctx, id);
  // Kept executable beneath test.fixme so implementing the verb turns this row into real coverage.
  await cli.stop(ctx, id);
});
