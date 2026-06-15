import { afterEach, describe, expect, it, vi } from "vitest";
import { runFork, runForkDelivery, ForkError } from "./fork";
import { deliverOwnerSealedJournalKeys } from "./migration";
import type { DeviceKeys } from "@/keys/device";

vi.mock("./migration", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./migration")>();
  return {
    ...actual,
    deliverOwnerSealedJournalKeys: vi.fn(),
  };
});

const DEVICE_KEYS = {
  x25519Private: {} as CryptoKey,
  x25519Public: {} as CryptoKey,
  ecdsaPrivate: {} as CryptoKey,
  ecdsaPublic: {} as CryptoKey,
} satisfies DeviceKeys;

function mockFetchSequence(jsons: unknown[]) {
  const calls: { url: string; body: any }[] = [];
  const fetchMock = vi.fn(async (url: string, init: any) => {
    calls.push({ url, body: JSON.parse(init.body) });
    const json = jsons.shift() ?? {};
    return new Response(JSON.stringify(json), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  });
  vi.stubGlobal("fetch", fetchMock);
  return calls;
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("runFork", () => {
  it("POSTs ForkSpawn with only spawnId for same-node default", async () => {
    const calls = mockFetchSequence([
      { forkSpawnId: "fork-1", nodeId: "node-a", transferSetId: "ts-1" },
      { entries: [] },
    ]);

    const res = await runFork("source-1", {}, null, "", new Date("2026-06-15T00:00:00Z"));

    expect(calls[0].url).toContain("/cp.v1.SpawnService/ForkSpawn");
    expect(calls[0].body).toEqual({ spawnId: "source-1", targetNodeId: "", targetClass: "", name: "" });
    expect(calls[1].url).toContain("/cp.v1.SpawnService/GetJournalKeyCiphertext");
    expect(calls[1].body).toEqual({ spawnId: "fork-1" });
    expect(res).toEqual({
      forkSpawnId: "fork-1",
      resolvedNodeId: "node-a",
      transferSetId: "ts-1",
      journalKeysDelivered: 0,
    });
  });

  it("does not load device keys when fork has no journal entries", async () => {
    const calls = mockFetchSequence([
      { forkSpawnId: "123e4567-e89b-12d3-a456-426614174000", nodeId: "node-a", transferSetId: "" },
      { entries: [] },
    ]);
    const loadKeys = vi.fn().mockResolvedValue(DEVICE_KEYS);

    const res = await runFork("source-1", {}, loadKeys, "", new Date("2026-06-15T00:00:00Z"));

    expect(calls[0].url).toContain("/cp.v1.SpawnService/ForkSpawn");
    expect(loadKeys).not.toHaveBeenCalled();
    expect(res.forkSpawnId).toBe("123e4567-e89b-12d3-a456-426614174000");
    expect(res.journalKeysDelivered).toBe(0);
  });

  it("loads device keys only after journal entries are found", async () => {
    vi.mocked(deliverOwnerSealedJournalKeys).mockResolvedValue({ journalKeysDelivered: 1 });
    mockFetchSequence([
      { forkSpawnId: "223e4567-e89b-12d3-a456-426614174000", nodeId: "node-b", transferSetId: "" },
      { entries: [{ mount: "workspace", ciphertext: "ciphertext" }] },
    ]);
    const loadKeys = vi.fn().mockResolvedValue(DEVICE_KEYS);

    await runFork("source-1", { nodeId: "node-b" }, loadKeys, "root", new Date("2026-06-15T00:00:00Z"));

    expect(loadKeys).toHaveBeenCalledTimes(1);
    expect(deliverOwnerSealedJournalKeys).toHaveBeenCalledWith(
      "223e4567-e89b-12d3-a456-426614174000",
      [{ mount: "workspace", ciphertext: "ciphertext" }],
      { nodeId: "node-b", class: "" },
      DEVICE_KEYS,
      "root",
      new Date("2026-06-15T00:00:00Z"),
      expect.any(Function),
      undefined,
    );
  });

  it("rejects node and class together before RPC", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    await expect(runFork("source-1", { nodeId: "node-a", class: "cloud" }, null, "", new Date()))
      .rejects.toThrow(/node or class/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("maps exact node, class, and trimmed name into ForkSpawn", async () => {
    const calls = mockFetchSequence([
      { forkSpawnId: "fork-2", nodeId: "node-b", transferSetId: "" },
      { entries: [] },
    ]);

    await runFork("source-1", { nodeId: "node-b" }, null, "", new Date(), " Trial ");
    expect(calls[0].body).toEqual({ spawnId: "source-1", targetNodeId: "node-b", targetClass: "", name: "Trial" });

    const classCalls = mockFetchSequence([
      { forkSpawnId: "fork-3", nodeId: "node-c", transferSetId: "" },
      { entries: [] },
    ]);
    await runFork("source-1", { class: "cloud" }, null, "", new Date(), "   ");
    expect(classCalls[0].body).toEqual({ spawnId: "source-1", targetNodeId: "", targetClass: "cloud", name: "" });
  });

  it("delivers owner-sealed journal keys to the returned fork spawn id", async () => {
    vi.mocked(deliverOwnerSealedJournalKeys).mockResolvedValue({ journalKeysDelivered: 1 });
    const calls = mockFetchSequence([
      { forkSpawnId: "fork-4", nodeId: "node-b", transferSetId: "ts-4" },
      { entries: [{ mount: "workspace", ciphertext: "ciphertext" }] },
    ]);

    const res = await runFork("source-1", { nodeId: "node-b" }, DEVICE_KEYS, "root", new Date("2026-06-15T00:00:00Z"));

    expect(calls[1].body).toEqual({ spawnId: "fork-4" });
    expect(deliverOwnerSealedJournalKeys).toHaveBeenCalledWith(
      "fork-4",
      [{ mount: "workspace", ciphertext: "ciphertext" }],
      { nodeId: "node-b", class: "" },
      DEVICE_KEYS,
      "root",
      new Date("2026-06-15T00:00:00Z"),
      expect.any(Function),
      undefined,
    );
    expect(res.journalKeysDelivered).toBe(1);
  });

  it("wraps delivery failures as ForkError with delivery leg", async () => {
    vi.mocked(deliverOwnerSealedJournalKeys).mockRejectedValue(new Error("node key rejected"));
    mockFetchSequence([
      { forkSpawnId: "fork-5", nodeId: "node-b", transferSetId: "ts-5" },
      { entries: [{ mount: "workspace", ciphertext: "ciphertext" }] },
    ]);

    await expect(runFork("source-1", { nodeId: "node-b" }, DEVICE_KEYS, "", new Date()))
      .rejects.toMatchObject({ name: "ForkError", leg: "delivery", message: expect.stringContaining("node key rejected") });
  });

  it("requires device keys when journal delivery is pending", async () => {
    mockFetchSequence([
      { forkSpawnId: "fork-6", nodeId: "node-b", transferSetId: "ts-6" },
      { entries: [{ mount: "workspace", ciphertext: "ciphertext" }] },
    ]);

    await expect(runFork("source-1", { nodeId: "node-b" }, null, "", new Date()))
      .rejects.toBeInstanceOf(ForkError);
  });

  it("runForkDelivery fetches pending fork entries and delivers them to the current target", async () => {
    vi.mocked(deliverOwnerSealedJournalKeys).mockResolvedValue({ journalKeysDelivered: 1 });
    const calls = mockFetchSequence([
      { entries: [{ mount: "workspace", ciphertext: "ciphertext" }] },
    ]);

    const res = await runForkDelivery(
      "fork-7",
      { nodeId: "node-current", class: "self-hosted" },
      DEVICE_KEYS,
      "root",
      new Date("2026-06-15T00:00:00Z"),
    );

    expect(calls[0].url).toContain("/cp.v1.SpawnService/GetJournalKeyCiphertext");
    expect(calls[0].body).toEqual({ spawnId: "fork-7" });
    expect(deliverOwnerSealedJournalKeys).toHaveBeenCalledWith(
      "fork-7",
      [{ mount: "workspace", ciphertext: "ciphertext" }],
      { nodeId: "node-current", class: "self-hosted" },
      DEVICE_KEYS,
      "root",
      new Date("2026-06-15T00:00:00Z"),
      undefined,
      undefined,
    );
    expect(res).toEqual({ journalKeysDelivered: 1 });
  });
});
