import { describe, it, expect } from "vitest";
import { reconcilePending, MAX_QUEUED } from "./turn";
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
