import { describe, it, expect, vi } from "vitest";
import { waitForStatus, GARAGE_HINT, type StatusOracle } from "./wait";
import type { SpawnSummary } from "../drivers/oracle";

function summary(status: SpawnSummary["status"]): SpawnSummary {
  return {
    spawnId: "sp-1",
    appId: "acc/app",
    appVersion: "1",
    model: "m",
    status,
    createdAt: "0",
    lastUsedAt: "0",
    name: "n",
    modelApplied: true,
  };
}

describe("waitForStatus", () => {
  it("resolves immediately if the oracle already reports the wanted status", async () => {
    const oracle: StatusOracle = { findSpawn: vi.fn().mockResolvedValue(summary("SUSPENDED")) };
    const got = await waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1 });
    expect(got.status).toBe("SUSPENDED");
  });

  it("polls through intermediate statuses until the wanted one is reached", async () => {
    const findSpawn = vi
      .fn()
      .mockResolvedValueOnce(summary("ACTIVE"))
      .mockResolvedValueOnce(summary("SUSPENDING"))
      .mockResolvedValueOnce(summary("SUSPENDED"));
    const oracle: StatusOracle = { findSpawn };
    const got = await waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1 });
    expect(got.status).toBe("SUSPENDED");
    expect(findSpawn.mock.calls.length).toBeGreaterThanOrEqual(3);
  });

  it("throws on timeout, naming the wanted status, the last-seen status, and the hint", async () => {
    const oracle: StatusOracle = { findSpawn: vi.fn().mockResolvedValue(summary("ACTIVE")) };
    await expect(waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1, timeoutMs: 5, timeoutHint: GARAGE_HINT })).rejects.toThrow(
      /SUSPENDED/,
    );
    await expect(waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1, timeoutMs: 5, timeoutHint: GARAGE_HINT })).rejects.toThrow(
      /ACTIVE/,
    );
    await expect(waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1, timeoutMs: 5, timeoutHint: GARAGE_HINT })).rejects.toThrow(
      /Garage/,
    );
  });

  it("rejects promptly (not waiting out the timeout) on a terminal status while waiting for a non-terminal target", async () => {
    const oracle: StatusOracle = { findSpawn: vi.fn().mockResolvedValue(summary("ERROR")) };
    await expect(waitForStatus(oracle, "sp-1", "SUSPENDED", { pollMs: 1, timeoutMs: 60_000 })).rejects.toThrow(/terminal status/);
  });
});
