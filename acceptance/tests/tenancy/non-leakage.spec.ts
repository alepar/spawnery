/**
 * Phase 6 — tenancy non-leakage: owner A never sees owner B's spawns, and vice versa, on both
 * the api (oracle) and cli surfaces. Uses its own dedicated ACC_TENANCY_A/B identities (falling
 * back to the shared ACC_IDENTITY_POOL's first two entries — see tenancy.ts / README.md),
 * bypassing the worker `identity` fixture entirely: this scenario is inherently two-owner, not
 * one-owner-per-worker.
 *
 * @mutating (creates spawns) — left UNTAGGED so the harness's default-deny guardrail treats it
 * as mutating and blocks it against a host that isn't on the non-prod allowlist.
 */

import { test, expect } from "../../src/harness/test";
import { AcceptanceClient } from "../../src/drivers/oracle";
import { CliDriver } from "../../src/drivers/cli";
import { loadTenancyConfig } from "../../src/scenarios/tenancy";
import type { DriverCtx } from "../../src/drivers/types";

test("owner A sees only A's spawns; owner B sees only B's (api + cli)", async ({ target, ns }) => {
  const cfg = loadTenancyConfig();

  const apiA = new AcceptanceClient({ baseUrl: target.cpEndpoint, bearer: cfg.a.token });
  const apiB = new AcceptanceClient({ baseUrl: target.cpEndpoint, bearer: cfg.b.token });
  const cli = new CliDriver({ cpEndpoint: target.cpEndpoint, spawnctlBin: target.spawnctlBin });
  const ctxA: DriverCtx = { identity: cfg.a, ns, api: apiA };
  const ctxB: DriverCtx = { identity: cfg.b, ns, api: apiB };

  const idA = await apiA.createSpawn({ appId: cfg.appId, model: cfg.model, name: ns("tenancy-a") });
  const idB = await apiB.createSpawn({ appId: cfg.appId, model: cfg.model, name: ns("tenancy-b") });

  try {
    const [listA, listB] = await Promise.all([apiA.listSpawns(), apiB.listSpawns()]);
    expect(listA).toContainSpawn(idA);
    expect(listA).not.toContainSpawn(idB);
    expect(listB).toContainSpawn(idB);
    expect(listB).not.toContainSpawn(idA);

    const [cliListA, cliListB] = await Promise.all([cli.list(ctxA), cli.list(ctxB)]);
    expect(cliListA.some((s) => s.spawnId === idA)).toBe(true);
    expect(cliListA.some((s) => s.spawnId === idB)).toBe(false);
    expect(cliListB.some((s) => s.spawnId === idB)).toBe(true);
    expect(cliListB.some((s) => s.spawnId === idA)).toBe(false);
  } finally {
    await apiA.deleteSpawn(idA).catch(() => {});
    await apiB.deleteSpawn(idB).catch(() => {});
  }
});
