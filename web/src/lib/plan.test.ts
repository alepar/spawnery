import { describe, it, expect } from "vitest";
import { upsertPlan } from "./plan";
import type { PlanEntry } from "@/acp/frames";
import type { Item } from "@/views/chat/types";

type PlanItem = Extract<Item, { kind: "plan" }>;

// Deterministic id minter so assertions can check plan-item identity / count.
function minter(start = 1) {
  let n = start;
  return () => n++;
}

const e = (content: string, status: string, priority = "medium"): PlanEntry => ({ content, status, priority });

describe("upsertPlan", () => {
  it("creates a single plan item from the first plan frame, by status", () => {
    const entries = [e("design", "completed"), e("build", "in_progress"), e("test", "pending")];
    const out = upsertPlan([], entries, minter());
    expect(out).toHaveLength(1);
    const plan = out[0] as PlanItem;
    expect(plan.kind).toBe("plan");
    expect(plan.id).toBe(1);
    expect(plan.entries.map((x) => x.status)).toEqual(["completed", "in_progress", "pending"]);
    expect(plan.entries.map((x) => x.content)).toEqual(["design", "build", "test"]);
  });

  it("REPLACES the prior plan in place on a second frame (does not append/stack)", () => {
    const first = upsertPlan([], [e("design", "in_progress"), e("build", "pending")], minter());
    // Second frame: the whole list re-sent, with entries advanced.
    const out = upsertPlan(first, [e("design", "completed"), e("build", "in_progress")], minter(99));

    expect(out).toHaveLength(1); // still exactly one plan item — not two
    const plan = out[0] as PlanItem;
    expect(plan.id).toBe(1); // same Item id/position, minter(99) NOT used (replace, not create)
    expect(plan.entries.map((x) => x.status)).toEqual(["completed", "in_progress"]);
  });

  it("preserves plan position relative to other items when replacing", () => {
    const id = minter();
    let items: Item[] = [{ kind: "user", text: "hi", id: id() }];
    items = upsertPlan(items, [e("a", "pending")], id);
    items = [...items, { kind: "agent", text: "ok", id: id() }];
    // Replace the plan — it must stay at index 1, between the user and agent items.
    items = upsertPlan(items, [e("a", "completed")], id);

    expect(items.map((it) => it.kind)).toEqual(["user", "plan", "agent"]);
    expect((items[1] as PlanItem).entries[0].status).toBe("completed");
  });

  it("renders nothing for graceful absence: an empty plan frame with no prior plan is a no-op", () => {
    const out = upsertPlan([], [], minter());
    expect(out).toHaveLength(0); // no plan item created -> the checklist never appears
  });

  it("clears the plan in place when an empty plan frame follows a real plan", () => {
    const first = upsertPlan([], [e("a", "pending")], minter());
    const out = upsertPlan(first, [], minter(99));
    expect(out).toHaveLength(1); // item kept (so the component renders nothing for zero entries)
    expect((out[0] as PlanItem).entries).toHaveLength(0);
  });
});
