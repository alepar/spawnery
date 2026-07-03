/**
 * Phase 6 — tenancy quota: from a swept-clean, dedicated known-cap owner, exactly N CreateSpawn
 * calls succeed and the (N+1)th is rejected with ResourceExhausted (HTTP 429 /
 * resource_exhausted). Runs ONLY when ACC_QUOTA_CAP is a positive integer and ACC_QUOTA_TOKEN is
 * set — per the design, "a black-box suite can neither set nor reliably discover" a target's
 * per-owner cap (internal/cp/server.go's checkSpawnQuota treats maxSpawnsPerOwner<=0 as
 * unlimited), so this is a legitimate config gate, not a down-dependency skip.
 *
 * retries: 0 — this test mutates owner-global state (creates cap spawns); an infra retry
 * mid-mutation could double the count. It starts by sweeping the owner clean, so a fresh attempt
 * is deterministic on its own, but disabling retries avoids ever interleaving a retry with a
 * half-built state.
 */

import { test, expect } from "../../src/harness/test";
import { ApiDriver } from "../../src/drivers/api";
import { loadTenancyConfig, rawCreateSpawn, isResourceExhausted } from "../../src/scenarios/tenancy";

test.describe("quota (known-cap owner only)", () => {
  test.describe.configure({ retries: 0 });

  test("Nth create succeeds, N+1 is ResourceExhausted", async ({ target, ns }) => {
    const cfg = loadTenancyConfig();
    test.skip(!cfg.quota, "ACC_QUOTA_CAP unset/<=0 — quota only runs on a known-cap target");
    const q = cfg.quota!;
    const apiQ = new ApiDriver(target.cpEndpoint, q.identity.token);

    // Sweep the dedicated owner clean — a deterministic count requires starting from zero, and
    // this owner is dedicated to the quota test (never shared with a worker), so deleting
    // everything it owns is safe.
    const existing = await apiQ.listSpawns();
    await Promise.all(existing.map((s) => apiQ.deleteSpawn(s.spawnId).catch(() => {})));
    const afterSweep = await apiQ.listSpawns();
    expect(afterSweep, `quota owner ${q.owner} did not sweep clean — is it dedicated to this test?`).toHaveLength(0);

    const created: string[] = [];
    try {
      for (let i = 0; i < q.cap; i++) {
        created.push(await apiQ.createSpawn({ appId: cfg.appId, model: cfg.model, name: ns(`quota-${i}`) }));
      }
      expect(await apiQ.listSpawns()).toHaveLength(q.cap);

      const res = await rawCreateSpawn(target.cpEndpoint, q.identity.token, {
        appId: cfg.appId,
        model: cfg.model,
        name: ns("quota-overflow"),
      });
      expect(isResourceExhausted(res), `expected 429 resource_exhausted, got status=${res.status} code=${res.code} message=${res.message}`).toBe(
        true,
      );
    } finally {
      await Promise.all(created.map((id) => apiQ.deleteSpawn(id).catch(() => {})));
      // Defensive: if the N+1 create unexpectedly succeeded, it leaked a spawn — find and delete it.
      const overflow = await apiQ.listSpawns();
      const leaked = overflow.find((s) => s.name === ns("quota-overflow"));
      if (leaked) await apiQ.deleteSpawn(leaked.spawnId).catch(() => {});
    }
  });
});
