import { describe, expect, it, vi } from "vitest";
import { runTemporarySPALifecycle } from "./temporary-spa-lifecycle";

function successfulStep(order: string[], name: string) {
  return vi.fn(async () => { order.push(name); });
}

describe("temporary SPA lifecycle", () => {
  it("attempts every recovery step after partial publication and aggregates failures in order", async () => {
    const order: string[] = [];
    const primary = new Error("partial publication failed");
    const restoration = new Error("restoration failed");
    const removal = new Error("temporary removal failed");
    const health = new Error("restored health failed");
    const reload = new Error("restored reload failed");
    const runWithTemporary = successfulStep(order, "body");

    const result = runTemporarySPALifecycle({
      publishTemporary: vi.fn(async () => { order.push("publish"); throw primary; }),
      runWithTemporary,
      restoreOriginal: vi.fn(async () => { order.push("restore"); throw restoration; }),
      removeTemporary: vi.fn(async () => { order.push("remove"); throw removal; }),
      verifyRestoredHealth: vi.fn(async () => { order.push("health"); throw health; }),
      reloadRestoredPage: vi.fn(async () => { order.push("reload"); throw reload; }),
    });

    const aggregate = await result.catch((reason) => reason) as AggregateError;
    expect(aggregate).toBeInstanceOf(AggregateError);
    expect(aggregate.errors).toEqual([primary, restoration, removal, health, reload]);
    expect(aggregate.errors[0]).toBe(primary);
    expect(aggregate.errors[1]).toBe(restoration);
    expect(aggregate.errors[2]).toBe(removal);
    expect(aggregate.errors[3]).toBe(health);
    expect(aggregate.errors[4]).toBe(reload);
    expect(order).toEqual(["publish", "restore", "remove", "health", "reload"]);
    expect(runWithTemporary).not.toHaveBeenCalled();
  });

  it("preserves a primary body failure alongside restoration failure", async () => {
    const order: string[] = [];
    const primary = new Error("alternate body failed");
    const restoration = new Error("restoration failed");

    const aggregate = await runTemporarySPALifecycle({
      publishTemporary: successfulStep(order, "publish"),
      runWithTemporary: vi.fn(async () => { order.push("body"); throw primary; }),
      restoreOriginal: vi.fn(async () => { order.push("restore"); throw restoration; }),
      removeTemporary: successfulStep(order, "remove"),
      verifyRestoredHealth: successfulStep(order, "health"),
      reloadRestoredPage: successfulStep(order, "reload"),
    }).catch((reason) => reason) as AggregateError;

    expect(aggregate).toBeInstanceOf(AggregateError);
    expect(aggregate.errors).toEqual([primary, restoration]);
    expect(aggregate.errors[0]).toBe(primary);
    expect(aggregate.errors[1]).toBe(restoration);
    expect(order).toEqual(["publish", "body", "restore", "remove", "health", "reload"]);
  });

  it("reports cleanup-only failure and still completes later verification", async () => {
    const order: string[] = [];
    const cleanup = new Error("temporary removal failed");

    const result = runTemporarySPALifecycle({
      publishTemporary: successfulStep(order, "publish"),
      runWithTemporary: successfulStep(order, "body"),
      restoreOriginal: successfulStep(order, "restore"),
      removeTemporary: vi.fn(async () => { order.push("remove"); throw cleanup; }),
      verifyRestoredHealth: successfulStep(order, "health"),
      reloadRestoredPage: successfulStep(order, "reload"),
    });

    const aggregate = await result.catch((reason) => reason) as AggregateError;
    expect(aggregate).toBeInstanceOf(AggregateError);
    expect(aggregate.errors).toEqual([cleanup]);
    expect(aggregate.errors[0]).toBe(cleanup);
    expect(order).toEqual(["publish", "body", "restore", "remove", "health", "reload"]);
  });

  it("resolves without inventing a lifecycle failure when every step succeeds", async () => {
    const order: string[] = [];

    await expect(runTemporarySPALifecycle({
      publishTemporary: successfulStep(order, "publish"),
      runWithTemporary: successfulStep(order, "body"),
      restoreOriginal: successfulStep(order, "restore"),
      removeTemporary: successfulStep(order, "remove"),
      verifyRestoredHealth: successfulStep(order, "health"),
      reloadRestoredPage: successfulStep(order, "reload"),
    })).resolves.toBeUndefined();
    expect(order).toEqual(["publish", "body", "restore", "remove", "health", "reload"]);
  });
});
