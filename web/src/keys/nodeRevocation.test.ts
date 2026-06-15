import { describe, expect, it, vi } from "vitest";

import { asNodeRevocationChecker } from "./nodeRevocation";

describe("asNodeRevocationChecker", () => {
  it("allows nodes absent from the AS list", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-b"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher, ttlMs: 30_000 });
    await expect(checker.check("node-a")).resolves.toBeUndefined();
  });

  it("rejects revoked nodes", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-a"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher, ttlMs: 30_000 });
    await expect(checker.check("node-a")).rejects.toThrow("node is on the AS revocation deny-list");
  });

  it("fails closed on AS outage", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response("down", { status: 503 }));
    const checker = asNodeRevocationChecker({ fetcher, ttlMs: 30_000 });
    await expect(checker.check("node-a")).rejects.toThrow("revocation list unavailable");
  });

  it("reuses a successful response until the TTL expires", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-a"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher, ttlMs: 30_000, nowMs: () => 1_000 });
    await expect(checker.check("node-a")).rejects.toThrow("node is on the AS revocation deny-list");
    await expect(checker.check("node-b")).resolves.toBeUndefined();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
