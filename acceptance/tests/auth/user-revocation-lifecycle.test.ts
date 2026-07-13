import { readFileSync } from "node:fs";
import { describe, expect, it, vi } from "vitest";
import {
  aggregateLifecycleFailures,
  cleanupSpawnFailures,
} from "./user-revocation-lifecycle";

describe("revocation lifecycle failure aggregation", () => {
  it("turns cleanup-only failure into an AggregateError without replacing the cause", () => {
    const cleanupError = new Error("cleanup failed");

    const aggregate = aggregateLifecycleFailures(undefined, undefined, [cleanupError]);

    expect(aggregate).toBeInstanceOf(AggregateError);
    expect(aggregate?.errors).toEqual([cleanupError]);
    expect(aggregate?.errors[0]).toBe(cleanupError);
  });

  it("preserves primary, restoration, and every cleanup error in order", () => {
    const primary = new Error("primary failed");
    const restoration = new Error("restoration failed");
    const cleanupOne = new Error("cleanup one failed");
    const cleanupTwo = new Error("cleanup two failed");

    const aggregate = aggregateLifecycleFailures(
      primary,
      restoration,
      [cleanupOne, cleanupTwo],
    );

    expect(aggregate?.errors).toEqual([primary, restoration, cleanupOne, cleanupTwo]);
    expect(aggregate?.errors[0]).toBe(primary);
    expect(aggregate?.errors[1]).toBe(restoration);
    expect(aggregate?.errors[2]).toBe(cleanupOne);
    expect(aggregate?.errors[3]).toBe(cleanupTwo);
  });

  it("attempts every deletion and returns every rejection reason", async () => {
    const cleanupOne = new Error("cleanup one failed");
    const cleanupThree = new Error("cleanup three failed");
    const deleteSpawn = vi.fn(async (spawnId: string) => {
      if (spawnId === "spawn-1") throw cleanupOne;
      if (spawnId === "spawn-3") throw cleanupThree;
    });

    const failures = await cleanupSpawnFailures(
      ["spawn-1", "spawn-2", "spawn-3"],
      deleteSpawn,
    );

    expect(deleteSpawn.mock.calls.map(([spawnId]) => spawnId)).toEqual([
      "spawn-1",
      "spawn-2",
      "spawn-3",
    ]);
    expect(failures).toEqual([cleanupOne, cleanupThree]);
    expect(failures[0]).toBe(cleanupOne);
    expect(failures[1]).toBe(cleanupThree);
  });

  it("routes spec cleanup and terminal failure through the aggregation helpers", () => {
    const source = readFileSync(
      new URL("./user-revocation-lifecycle.spec.ts", import.meta.url),
      "utf8",
    );

    expect(source).not.toContain(".catch(() => {})");
    expect(source).toMatch(/cleanupErrors\s*=\s*await cleanupSpawnFailures/);
    expect(source).toMatch(/aggregateLifecycleFailures\(testError, restorationError, cleanupErrors\)/);
  });
});
