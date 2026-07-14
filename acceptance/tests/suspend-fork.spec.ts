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
import { newMarker, writeMarker, readMarker, assertFreshMarker } from "../src/scenarios/marker";
import { waitForStatus, GARAGE_HINT } from "../src/scenarios/wait";

// Spawnable seed app id, a documented target precondition (see .env.example).
const appId = process.env.ACC_TEST_APP_ID ?? "opencode";

// suspend/resume across BOTH surfaces: the marker survives a full suspend (workspace → journal,
// pod torn down) + resume (journal → new pod). Proves the Garage journal moves the workspace.
// spawnctl gained a `suspend` verb (sp-6ag5.2), so cli is no longer a parity gap.
for (const surface of ["web", "cli"] as const) {
  test(`suspend/resume preserves per-run marker · ${surface}`, { tag: "@mutating" }, async ({ ctx, web, cli, api, runId }) => {
    const driver = surface === "web" ? web : cli;
    const cfg = cli.configuration();

    const id = await driver.createSpawn(ctx, { appId });
    await driver.waitActive(ctx, id);

    const marker = newMarker(runId, id);
    await writeMarker(cfg, ctx.identity, id, runId, marker);

    await driver.suspend(ctx, id);
    await waitForStatus(api, id, "SUSPENDED", { timeoutHint: GARAGE_HINT });

    await driver.resume(ctx, id);
    await driver.waitActive(ctx, id);
    expect(await api.listSpawns()).toContainSpawn(id, { status: "ACTIVE" });

    assertFreshMarker(await readMarker(cfg, ctx.identity, id, runId), marker);
  });
}

for (const surface of ["web", "cli"] as const) {
  test(`fork inherits per-run marker · ${surface}`, { tag: "@mutating" }, async ({ ctx, web, cli, api, ns, runId }) => {
    const driver = surface === "web" ? web : cli;
    const cfg = cli.configuration();

    const base = await driver.createSpawn(ctx, { appId });
    await driver.waitActive(ctx, base);

    const marker = newMarker(runId, base);
    await writeMarker(cfg, ctx.identity, base, runId, marker);

    const forkId = await driver.fork(ctx, base, { name: ns("fork") });
    await driver.waitActive(ctx, forkId);

    // Fork inherited the live workspace snapshot: the marker file is present AND fresh in the fork.
    assertFreshMarker(await readMarker(cfg, ctx.identity, forkId, runId), marker);

    const forkSummary = await api.findSpawn(forkId);
    expect(forkSummary?.parentSpawnId).toBe(base);
  });
}
