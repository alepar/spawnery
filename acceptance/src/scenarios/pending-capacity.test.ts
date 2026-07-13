import { describe, expect, it, vi } from "vitest";
import { cpv1 } from "@spawnery/client";
import { acquirePendingAfterCapacityConverges, PendingSpawnTerminalError } from "./pending-capacity";

function fakeClient(outcomes: Array<"capacity" | "other" | "ready">) {
  let current = "";
  let creates = 0;
  const deleted: string[] = [];
  return {
    client: {
      async createSpawn() {
        current = `spawn-${++creates}`;
        return { spawnId: current };
      },
      async getPendingIntent() {
        const outcome = outcomes[creates - 1];
        return outcome === "ready"
          ? { ready: true, pending: { spawnId: current } }
          : { ready: false };
      },
      async listSpawns() {
        const outcome = outcomes[creates - 1];
        if (outcome === "ready") return { spawns: [] };
        return { spawns: [{
          spawnId: current,
          status: cpv1.SpawnStatus.ERROR,
          errorStep: outcome === "capacity" ? "resolve placement" : "provision",
          errorDetail: outcome === "capacity"
            ? "resource_exhausted: no eligible node with capacity"
            : "failed_precondition: unrelated failure",
        }] };
      },
      async deleteSpawn({ spawnId }: { spawnId: string }) {
        deleted.push(spawnId);
        return {};
      },
    },
    deleted,
    createCount: () => creates,
  };
}

describe("acquirePendingAfterCapacityConverges", () => {
  it("deletes and retries only the exact post-cleanup capacity terminal", async () => {
    const fx = fakeClient(["capacity", "ready"]);
    const sleep = vi.fn(async () => undefined);

    const result = await acquirePendingAfterCapacityConverges(fx.client, { name: "probe" }, {
      maxAttempts: 3, pollAttempts: 1, pollMs: 0, retryMs: 25, sleep,
    });

    expect(result.created.spawnId).toBe("spawn-2");
    expect(result.pending.spawnId).toBe("spawn-2");
    expect(fx.deleted).toEqual(["spawn-1"]);
    expect(sleep).toHaveBeenCalledWith(25);
  });

  it("fails immediately with step and detail for every other terminal error", async () => {
    const fx = fakeClient(["other", "ready"]);

    await expect(acquirePendingAfterCapacityConverges(fx.client, { name: "probe" }, {
      maxAttempts: 3, pollAttempts: 1, pollMs: 0, retryMs: 0,
    })).rejects.toMatchObject({
      name: "PendingSpawnTerminalError",
      errorStep: "provision",
      errorDetail: "failed_precondition: unrelated failure",
    } satisfies Partial<PendingSpawnTerminalError>);
    expect(fx.createCount()).toBe(1);
    expect(fx.deleted).toEqual(["spawn-1"]);
  });

  it("caps retries and reports the final capacity terminal", async () => {
    const fx = fakeClient(["capacity", "capacity", "ready"]);

    await expect(acquirePendingAfterCapacityConverges(fx.client, { name: "probe" }, {
      maxAttempts: 2, pollAttempts: 1, pollMs: 0, retryMs: 0,
    })).rejects.toThrow(/capacity did not converge after 2 attempts/);
    expect(fx.createCount()).toBe(2);
    expect(fx.deleted).toEqual(["spawn-1", "spawn-2"]);
  });
});
