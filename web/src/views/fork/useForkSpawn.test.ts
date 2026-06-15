import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { listMigrationTargets } from "@/api/migration";
import { ForkError, runFork, runForkDelivery } from "@/api/fork";
import { loadDeviceKeys } from "@/keys/device";
import { useForkSpawn } from "./useForkSpawn";
import type { MigrationTarget } from "@/api/migration";
import type { DeviceKeys } from "@/keys/device";

vi.mock("@/api/migration", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/migration")>();
  return {
    ...actual,
    listMigrationTargets: vi.fn(),
  };
});

vi.mock("@/api/fork", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/fork")>();
  return {
    ...actual,
    runFork: vi.fn(),
    runForkDelivery: vi.fn(),
  };
});

vi.mock("@/keys/device", () => ({
  loadDeviceKeys: vi.fn(),
}));

vi.mock("@/config/trustAnchors", () => ({
  PINNED_ROOT_CA_PEM: "",
}));

const CURRENT_TARGET: MigrationTarget = {
  nodeId: "node-current",
  class: "self-hosted",
  yours: true,
  online: true,
  isCurrent: true,
  journalSizeBytes: 0,
};

const OTHER_TARGET: MigrationTarget = {
  nodeId: "node-b",
  class: "self-hosted",
  yours: true,
  online: true,
  isCurrent: false,
  journalSizeBytes: 0,
};

const FAKE_KEYS = {
  x25519Private: {} as CryptoKey,
  x25519Public: {} as CryptoKey,
  ecdsaPrivate: {} as CryptoKey,
  ecdsaPublic: {} as CryptoKey,
} satisfies DeviceKeys;

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
  vi.mocked(listMigrationTargets).mockResolvedValue({
    spawnDurabilityClass: "owner-sealed",
    targets: [CURRENT_TARGET, OTHER_TARGET],
  });
});

describe("useForkSpawn", () => {
  it("opens with ListMigrationTargets and keeps the current node selectable", async () => {
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });

    expect(listMigrationTargets).toHaveBeenCalledWith("source-1");
    expect(result.current.state.phase).toBe("selecting");
    expect(result.current.state.sourceSpawnId).toBe("source-1");
    expect(result.current.state.selectedTarget?.nodeId).toBe("node-current");
    expect(result.current.state.targets.map((t) => t.nodeId)).toEqual(["node-current", "node-b"]);
  });

  it("defaults to the first online target when current node is missing", async () => {
    vi.mocked(listMigrationTargets).mockResolvedValue({
      spawnDurabilityClass: "owner-sealed",
      targets: [{ ...CURRENT_TARGET, online: false }, OTHER_TARGET],
    });
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });

    expect(result.current.state.selectedTarget?.nodeId).toBe("node-b");
  });

  it("confirm calls runFork with same-node default and enters done", async () => {
    vi.mocked(runFork).mockResolvedValue({
      forkSpawnId: "fork-1",
      resolvedNodeId: "node-current",
      transferSetId: "ts-1",
      journalKeysDelivered: 0,
    });
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });
    act(() => { result.current.setName(" Trial "); });
    await act(async () => { await result.current.confirm(); });

    expect(runFork).toHaveBeenCalledWith("source-1", {}, expect.any(Function), "", expect.any(Date), " Trial ", expect.any(Function));
    expect(result.current.state.phase).toBe("done");
    expect(result.current.state.result?.forkSpawnId).toBe("fork-1");
  });

  it("does not load device keys before a no-delivery fork completes", async () => {
    vi.mocked(runFork).mockResolvedValue({
      forkSpawnId: "123e4567-e89b-12d3-a456-426614174000",
      resolvedNodeId: "node-current",
      transferSetId: "",
      journalKeysDelivered: 0,
    });
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });
    await act(async () => { await result.current.confirm(); });

    expect(loadDeviceKeys).not.toHaveBeenCalled();
    expect(result.current.state.phase).toBe("done");
  });

  it("delivery ForkError enters delivery-pending with the returned fork id", async () => {
    vi.mocked(runFork).mockRejectedValue(new ForkError("delivery failed", "delivery", "123e4567-e89b-12d3-a456-426614174000"));
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });
    await act(async () => { await result.current.confirm(); });

    expect(result.current.state.phase).toBe("delivery-pending");
    expect(result.current.state.forkSpawnId).toBe("123e4567-e89b-12d3-a456-426614174000");
    expect(result.current.state.errorMsg).toContain("delivery failed");
  });

  it("missing device keys during delivery enters needs-enroll", async () => {
    vi.mocked(runFork).mockRejectedValue(new ForkError(
      "Device enrollment required to deliver journal keys",
      "delivery",
      "223e4567-e89b-12d3-a456-426614174000",
    ));
    const { result } = renderHook(() => useForkSpawn());

    await act(async () => { await result.current.open("source-1"); });
    await act(async () => { await result.current.confirm(); });

    expect(result.current.state.phase).toBe("needs-enroll");
    expect(result.current.state.forkSpawnId).toBe("223e4567-e89b-12d3-a456-426614174000");
  });

  it("openDeliveryPending reconstructs a persisted pending fork", () => {
    const { result } = renderHook(() => useForkSpawn());

    act(() => { result.current.openDeliveryPending("fork-3"); });

    expect(result.current.state.phase).toBe("delivery-pending");
    expect(result.current.state.forkSpawnId).toBe("fork-3");
  });

  it("retryDelivery loads the current target and completes delivery", async () => {
    vi.mocked(runForkDelivery).mockResolvedValue({ journalKeysDelivered: 1 });
    const { result } = renderHook(() => useForkSpawn());
    act(() => { result.current.openDeliveryPending("323e4567-e89b-12d3-a456-426614174000"); });

    await act(async () => { await result.current.retryDelivery(); });

    expect(listMigrationTargets).toHaveBeenCalledWith("323e4567-e89b-12d3-a456-426614174000");
    expect(runForkDelivery).toHaveBeenCalledWith("323e4567-e89b-12d3-a456-426614174000", CURRENT_TARGET, FAKE_KEYS, "", expect.any(Date), expect.any(Function));
    expect(result.current.state.phase).toBe("done");
    expect(result.current.state.result?.forkSpawnId).toBe("323e4567-e89b-12d3-a456-426614174000");
    expect(result.current.state.result?.journalKeysDelivered).toBe(1);
  });
});
