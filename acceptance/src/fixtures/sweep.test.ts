import { describe, it, expect } from "vitest";
import { selectStale } from "./sweep";
import { newRunId, nsName } from "./namespace";
import type { SpawnSummary } from "../drivers/api";

function summary(name: string, createdAtSec = 0): SpawnSummary {
  return {
    spawnId: `id-${name}`,
    appId: "app",
    appVersion: "1",
    model: "m",
    status: "ACTIVE",
    createdAt: String(createdAtSec),
    lastUsedAt: String(createdAtSec),
    name,
    modelApplied: true,
  };
}

describe("selectStale — pre-run mode (no runId)", () => {
  const now = 10_000_000;
  const ttlMs = 60_000;
  const staleRunId = newRunId(now - ttlMs - 1000); // older than ttl
  const freshRunId = newRunId(now - 1000); // within ttl

  const rows = [
    summary(nsName(staleRunId, 0, "stale-spawn")),
    summary(nsName(freshRunId, 0, "fresh-spawn")),
    summary("not-namespaced-spawn"),
  ];

  it("returns only the stale acc- rows", () => {
    const stale = selectStale(rows, now, ttlMs);
    expect(stale.map((s) => s.name)).toEqual([nsName(staleRunId, 0, "stale-spawn")]);
  });

  it("excludes non-acc rows even if old", () => {
    const stale = selectStale(rows, now, ttlMs);
    expect(stale.some((s) => s.name === "not-namespaced-spawn")).toBe(false);
  });

  it("falls back to createdAt when the name has no parseable embedded timestamp", () => {
    const weird = summary("acc-not-a-real-runid-w0-thing", Math.floor((now - ttlMs - 5000) / 1000));
    const stale = selectStale([weird], now, ttlMs);
    expect(stale).toEqual([weird]);
  });
});

describe("selectStale — teardown mode (runId given)", () => {
  it("returns exactly this run's rows regardless of age", () => {
    const runId = newRunId(0); // very old, would be stale under any TTL
    const other = newRunId(0);
    const rows = [
      summary(nsName(runId, 0, "a")),
      summary(nsName(runId, 1, "b")),
      summary(nsName(other, 0, "c")),
      summary("not-namespaced"),
    ];
    const mine = selectStale(rows, Date.now(), 60_000, runId);
    expect(mine.map((s) => s.name).sort()).toEqual([nsName(runId, 0, "a"), nsName(runId, 1, "b")].sort());
  });
});
