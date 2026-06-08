import { describe, it, expect } from "vitest";
import { reconcilePending, MAX_QUEUED, turnEndLabel, usageBadge } from "./turn";
import type { Item } from "@/views/chat/types";

const u = (id: number, pending?: boolean): Item => ({ id, kind: "user", text: "x", pending });

describe("reconcilePending", () => {
  it("keeps exactly `queued` of the most recent pending user items pending", () => {
    const items: Item[] = [u(1, true), { id: 2, kind: "agent", text: "..." }, u(3, true), u(4, true)];
    const out = reconcilePending(items, 1);
    expect(out.filter((i) => i.kind === "user" && i.pending).map((i) => i.id)).toEqual([4]);
  });

  it("clears all pending when queued is 0", () => {
    const items: Item[] = [u(1, true), u(2, true)];
    expect(reconcilePending(items, 0).every((i) => !(i.kind === "user" && i.pending))).toBe(true);
  });

  it("MAX_QUEUED is a positive cap", () => {
    expect(MAX_QUEUED).toBeGreaterThan(0);
  });
});

describe("turnEndLabel", () => {
  it("returns null for a normal end_turn (nothing to show)", () => {
    expect(turnEndLabel({})).toBeNull();
    expect(turnEndLabel({ reason: "end_turn" })).toBeNull();
  });

  it("maps non-normal stop reasons to labels", () => {
    expect(turnEndLabel({ reason: "max_tokens" })).toBe("stopped: max tokens");
    expect(turnEndLabel({ reason: "max_turn_requests" })).toBe("stopped: max requests");
    expect(turnEndLabel({ reason: "refusal" })).toBe("refused");
    expect(turnEndLabel({ reason: "cancelled" })).toBe("cancelled");
  });

  it("prefers a structured error message over the reason", () => {
    expect(turnEndLabel({ reason: "end_turn", error: { message: "missing api key" } })).toBe("error: missing api key");
  });
});

describe("usageBadge", () => {
  it("returns null when usage is absent (graceful absence for non-reporting agents)", () => {
    expect(usageBadge(undefined)).toBeNull();
    expect(usageBadge({})).toBeNull();
  });

  it("shows a compact token count (input+output), using total when present", () => {
    expect(usageBadge({ input: 100, output: 50 })).toBe("150 tokens");
    expect(usageBadge({ input: 8000, output: 4300, total: 12300 })).toBe("12.3k tokens");
  });

  it("appends cost only when priced (>0), and never renders $0.00", () => {
    expect(usageBadge({ total: 1500, cost: 0.04 })).toBe("1.5k tokens · $0.0400");
    expect(usageBadge({ total: 1500, cost: 4.2 })).toBe("1.5k tokens · $4.20");
    expect(usageBadge({ total: 1500 })).toBe("1.5k tokens"); // unpriced -> no cost segment
    expect(usageBadge({ total: 1500, cost: 0 })).toBe("1.5k tokens"); // zero cost -> no $0.00
  });

  it("returns null when there is nothing meaningful (zero tokens, no cost)", () => {
    expect(usageBadge({ input: 0, output: 0, cost: 0 })).toBeNull();
  });

  it("can show a cost-only badge when tokens are absent but the turn was priced", () => {
    expect(usageBadge({ cost: 0.05 })).toBe("$0.0500");
  });
});
