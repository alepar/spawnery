import { describe, expect, it, vi } from "vitest";

import { asNodeRevocationChecker } from "./nodeRevocation";

describe("asNodeRevocationChecker", () => {
  it("allows nodes absent from the AS list", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-b"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher });
    await expect(checker.check("node-a")).resolves.toBeUndefined();
  });

  it("rejects revoked nodes", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-a"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher });
    await expect(checker.check("node-a")).rejects.toThrow("node is on the AS revocation deny-list");
  });

  it("fails closed on AS outage", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response("down", { status: 503 }));
    const checker = asNodeRevocationChecker({ fetcher });
    await expect(checker.check("node-a")).rejects.toThrow("revocation list unavailable");
  });

  it("fails closed on missing or null revoked_node_ids", async () => {
    for (const body of [{}, { revoked_node_ids: null }]) {
      const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 }));
      const checker = asNodeRevocationChecker({ fetcher });
      await expect(checker.check("node-a")).rejects.toThrow("revocation list unavailable");
    }
  });

  it("refetches by default and fails closed if the second AS check is unavailable", async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ revoked_node_ids: [] }), { status: 200 }))
      .mockResolvedValueOnce(new Response("down", { status: 503 }));
    const checker = asNodeRevocationChecker({ fetcher });
    await expect(checker.check("node-a")).resolves.toBeUndefined();
    await expect(checker.check("node-a")).rejects.toThrow("revocation list unavailable");
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it("reuses a successful response when a TTL cache is explicitly opted in", async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify({ revoked_node_ids: ["node-a"] }), { status: 200 }));
    const checker = asNodeRevocationChecker({ fetcher, ttlMs: 30_000, nowMs: () => 1_000 });
    await expect(checker.check("node-a")).rejects.toThrow("node is on the AS revocation deny-list");
    await expect(checker.check("node-b")).resolves.toBeUndefined();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
