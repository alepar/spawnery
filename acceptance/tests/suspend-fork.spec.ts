/**
 * Phase 3: suspend/resume + fork (per-run marker survives).
 *
 * Proves journaled suspend/resume and fork preserve a spawn's workspace state on a live target,
 * by writing a per-run unique marker via `spawnctl exec`, mutating the spawn, and asserting the
 * marker reads back FRESH (equal to this run's value — not merely "the file exists", which a
 * stale marker from a prior run would satisfy). See design §Assertion strategy.
 *
 * These operations are journal/blob-store backed (Garage) — waitForStatus's timeout hints at that
 * dependency rather than a separate probe RPC (design §Risks: "fail loudly if absent").
 */

import { test, expect } from "../src/harness/test";
import { execConfigFromTarget } from "../src/scenarios/exec";
import { newMarker, writeMarker, readMarker, assertFreshMarker } from "../src/scenarios/marker";
import { waitForStatus, GARAGE_HINT } from "../src/scenarios/wait";

// Spawnable seed app id, a documented target precondition (see .env.example).
const appId = process.env.ACC_TEST_APP_ID ?? "opencode";

test("suspend/resume preserves per-run marker · web", { tag: "@mutating" }, async ({ ctx, web, api, runId, target }) => {
  const cfg = execConfigFromTarget(target);

  const id = await web.createSpawn(ctx, { appId });
  await web.waitActive(ctx, id);

  const marker = newMarker(runId, id);
  await writeMarker(cfg, id, runId, marker);

  await web.suspend(ctx, id);
  await waitForStatus(api, id, "SUSPENDED", { timeoutHint: GARAGE_HINT });

  await web.resume(ctx, id);
  await web.waitActive(ctx, id);
  expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });

  assertFreshMarker(await readMarker(cfg, id, runId), marker);
});

test("cli suspend is a known parity gap (expected fail) · cli", { tag: "@mutating" }, async ({ ctx, cli }) => {
  // spawnctl has no `suspend` verb (CliDriver stub throws parityGap). Tracked as product debt —
  // when spawnctl gains suspend this test starts PASSING, Playwright flags "unexpected pass",
  // and that is the signal to update this row and drop test.fail(). See the follow-up bead filed
  // alongside this task (spawnctl suspend cli parity gap).
  test.fail();
  await cli.suspend(ctx, "sp-nonexistent"); // throws synchronously → satisfies test.fail()
});

for (const surface of ["web", "cli"] as const) {
  test(`fork inherits per-run marker · ${surface}`, { tag: "@mutating" }, async ({ ctx, web, cli, api, ns, runId, target }) => {
    const driver = surface === "web" ? web : cli;
    const cfg = execConfigFromTarget(target);

    const base = await driver.createSpawn(ctx, { appId });
    await driver.waitActive(ctx, base);

    const marker = newMarker(runId, base);
    await writeMarker(cfg, base, runId, marker);

    const forkId = await driver.fork(ctx, base, { name: ns("fork") });
    await driver.waitActive(ctx, forkId);

    // Fork inherited the live workspace snapshot: the marker file is present AND fresh in the fork.
    assertFreshMarker(await readMarker(cfg, forkId, runId), marker);

    const forkSummary = await api.findSpawn(forkId);
    expect(forkSummary?.parentSpawnId).toBe(base);
  });
}
