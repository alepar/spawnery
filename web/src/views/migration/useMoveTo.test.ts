/**
 * Vitest for the useMoveTo state machine and the MoveToModal error legs.
 *
 * Covers (spec §3, §4):
 *   - Full happy-path: idle → loading → selecting → confirming → running → done
 *   - Suspend-leg failure  → error-suspend phase with Recreate action
 *   - Resume-leg failure   → error-resume phase with Resume-on-origin action
 *   - Delivery-leg failure → delivery-pending phase (persistent, reload-derivable)
 *   - Network unreachable  → reconnecting phase with retry
 *   - Enrollment check preflight: unenrolled → needs-enroll phase (spawn untouched)
 *   - Node-local upgrade pivot: upgrading phase → owner-sealed → done
 *   - Minimize / restore (WM14)
 *   - openDeliveryPending reconstruction (spec §3 "reload reconstructs modal state")
 *   - Revoked-node refusal via MigrateError delivery leg (WM8)
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useMoveTo } from "./useMoveTo";
import { MigrateError } from "@/api/migration";

// ── Module mocks ─────────────────────────────────────────────────────────────

vi.mock("@/api/migration", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/migration")>();
  return {
    ...actual,
    listMigrationTargets: vi.fn(),
    getJournalKeyCiphertext: vi.fn(),
    classifyDurability: vi.fn(),
    runMigrate: vi.fn(),
    upgradeToOwnerSealed: vi.fn(),
  };
});

vi.mock("@/keys/device", () => ({
  loadDeviceKeys: vi.fn(),
}));

vi.mock("@/config/trustAnchors", () => ({
  PINNED_ROOT_CA_PEM: "",
}));

import * as migrationMod from "@/api/migration";
import * as deviceMod from "@/keys/device";

// ── Helpers ──────────────────────────────────────────────────────────────────

const TARGET_A = {
  nodeId: "node-a",
  class: "self-hosted" as const,
  yours: true,
  online: true,
  isCurrent: false,
  journalSizeBytes: 2 * 1024 * 1024,
};

const TARGET_CURRENT = {
  ...TARGET_A,
  nodeId: "node-current",
  isCurrent: true,
};

const ENTRY = {
  mount: "workspace",
  ciphertext: "Y2lwaGVydGV4dA==",
};

const FAKE_KEYS = {
  x25519Private: {} as CryptoKey,
  x25519Public: { type: "public" } as CryptoKey,
  ecdsaPrivate: {} as CryptoKey,
  ecdsaPublic: {} as CryptoKey,
};

/** Stub crypto.subtle.exportKey for tests that exercise owner-sealed / upgrade paths. */
function stubExportKey() {
  vi.stubGlobal("crypto", {
    ...globalThis.crypto,
    subtle: {
      ...globalThis.crypto.subtle,
      exportKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
    },
    randomUUID: () => "test-uuid",
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("useMoveTo state machine", () => {
  it("starts idle", () => {
    const { result } = renderHook(() => useMoveTo());
    expect(result.current.state.phase).toBe("idle");
    expect(result.current.state.minimized).toBe(false);
  });

  it("happy path: idle → selecting → confirming → done (ephemeral)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A, TARGET_CURRENT], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockResolvedValue({
      resolvedNodeId: "node-a",
      journalKeysDelivered: 0,
    });

    const { result } = renderHook(() => useMoveTo());

    await act(async () => { result.current.open("spawn-1"); });
    expect(result.current.state.phase).toBe("selecting");
    // Current node is filtered out.
    expect(result.current.state.targets).toHaveLength(1);
    expect(result.current.state.targets[0].nodeId).toBe("node-a");

    act(() => { result.current.select(TARGET_A); });
    expect(result.current.state.phase).toBe("confirming");
    expect(result.current.state.selectedTarget?.nodeId).toBe("node-a");

    await act(async () => { result.current.confirm(); });
    expect(result.current.state.phase).toBe("done");
    expect(result.current.state.result?.resolvedNodeId).toBe("node-a");
    expect(result.current.state.result?.journalKeysDelivered).toBe(0);
  });

  it("happy path: owner-sealed with journal key delivery", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "owner-sealed" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([ENTRY]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("owner-sealed");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockResolvedValue({
      resolvedNodeId: "node-a",
      journalKeysDelivered: 1,
    });

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-2"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("done");
    expect(result.current.state.result?.journalKeysDelivered).toBe(1);
  });

  it("cancel resets to idle", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-3"); });
    expect(result.current.state.phase).toBe("selecting");

    act(() => { result.current.cancel(); });
    expect(result.current.state.phase).toBe("idle");
    expect(result.current.state.spawnId).toBeNull();
  });
});

describe("useMoveTo per-leg error states (WM3)", () => {
  it("suspend-leg failure → error-suspend phase", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockRejectedValue(
      new MigrateError("suspend failed: node unreachable", "suspend"),
    );

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-s"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("error-suspend");
    expect(result.current.state.errorMsg).toContain("suspend failed");
  });

  it("resume-leg failure → error-resume phase", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockRejectedValue(
      new MigrateError("target node failed to resume", "resume"),
    );

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-r"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("error-resume");
    expect(result.current.state.errorMsg).toContain("resume");
  });

  it("delivery-leg failure → delivery-pending phase (WM3 / WM8 revocation path)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([ENTRY]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("owner-sealed");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    // Simulate delivery failure — including the revoked-node case (delivery leg, WM8).
    vi.mocked(migrationMod.runMigrate).mockRejectedValue(
      new MigrateError("Node verification failed: node is on the AS revocation deny-list", "delivery"),
    );

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-d"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("delivery-pending");
    expect(result.current.state.errorMsg).toContain("revocation deny-list");
  });

  it("network error → reconnecting phase", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockRejectedValue(
      new MigrateError("failed to fetch: network error", "network"),
    );

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-n"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("reconnecting");
  });

  it("open() network error → reconnecting (CP unreachable during target load)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockRejectedValue(new Error("fetch failed"));

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-x"); });

    expect(result.current.state.phase).toBe("reconnecting");
    expect(result.current.state.errorMsg).toContain("fetch failed");
  });
});

describe("useMoveTo enrollment check preflight", () => {
  it("owner-sealed + unenrolled browser → needs-enroll (spawn untouched)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([ENTRY]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("owner-sealed");
    // Simulate unenrolled browser.
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(null);

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-e"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("needs-enroll");
    // runMigrate must NOT have been called (spawn untouched).
    expect(migrationMod.runMigrate).not.toHaveBeenCalled();
  });

  it("node-local + unenrolled browser → needs-enroll before upgrade", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("node-local");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(null);

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-nl"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("needs-enroll");
    expect(migrationMod.upgradeToOwnerSealed).not.toHaveBeenCalled();
    expect(migrationMod.runMigrate).not.toHaveBeenCalled();
  });
});

describe("useMoveTo node-local upgrade pivot (WM16)", () => {
  it("node-local → upgrading → done after upgrade + migrate", async () => {
    stubExportKey();
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("node-local");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.upgradeToOwnerSealed).mockResolvedValue(undefined);
    vi.mocked(migrationMod.runMigrate).mockResolvedValue({
      resolvedNodeId: "node-a",
      journalKeysDelivered: 0,
    });

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-up"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    // upgradeToOwnerSealed must have been called.
    expect(migrationMod.upgradeToOwnerSealed).toHaveBeenCalledWith(
      "spawn-up",
      expect.any(Array),
    );
    expect(result.current.state.phase).toBe("done");
  });

  it("node-local upgrade failure → reconnecting phase", async () => {
    stubExportKey();
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("node-local");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.upgradeToOwnerSealed).mockRejectedValue(new Error("CP rejected upgrade"));

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-upf"); });
    act(() => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });

    expect(result.current.state.phase).toBe("reconnecting");
    expect(result.current.state.errorMsg).toContain("CP rejected upgrade");
    expect(migrationMod.runMigrate).not.toHaveBeenCalled();
  });
});

describe("useMoveTo minimize / restore (WM14)", () => {
  it("minimize sets minimized=true; restore sets minimized=false from selecting phase", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-m"); });
    expect(result.current.state.phase).toBe("selecting");

    // minimize() is available regardless of phase; it just sets the minimized flag.
    await act(async () => { result.current.minimize(); });
    expect(result.current.state.minimized).toBe(true);

    await act(async () => { result.current.restore(); });
    expect(result.current.state.minimized).toBe(false);
  });

  it("minimize survives a done phase", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("ephemeral");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);
    vi.mocked(migrationMod.runMigrate).mockResolvedValue({ resolvedNodeId: "node-a", journalKeysDelivered: 0 });

    const { result } = renderHook(() => useMoveTo());
    await act(async () => { result.current.open("spawn-mm"); });
    await act(async () => { result.current.select(TARGET_A); });
    await act(async () => { result.current.confirm(); });
    expect(result.current.state.phase).toBe("done");

    await act(async () => { result.current.minimize(); });
    expect(result.current.state.minimized).toBe(true);

    await act(async () => { result.current.restore(); });
    expect(result.current.state.minimized).toBe(false);
  });
});

describe("useMoveTo delivery-pending reconstruction (spec §3)", () => {
  it("openDeliveryPending sets delivery-pending phase directly", () => {
    const { result } = renderHook(() => useMoveTo());

    act(() => { result.current.openDeliveryPending("spawn-dp"); });

    expect(result.current.state.phase).toBe("delivery-pending");
    expect(result.current.state.spawnId).toBe("spawn-dp");
    expect(result.current.state.errorMsg).toMatch(/not yet delivered/i);
  });

  it("retryDelivery in delivery-pending → loading → selecting (entries still present)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([ENTRY]);
    vi.mocked(migrationMod.classifyDurability).mockReturnValue("owner-sealed");
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);

    const { result } = renderHook(() => useMoveTo());
    act(() => { result.current.openDeliveryPending("spawn-dp2"); });
    await act(async () => { result.current.retryDelivery(); });

    expect(result.current.state.phase).toBe("selecting");
    expect(result.current.state.entries).toHaveLength(1);
  });

  it("retryDelivery when CP reports no entries → done (delivered on another device)", async () => {
    vi.mocked(migrationMod.listMigrationTargets).mockResolvedValue({ targets: [TARGET_A], spawnDurabilityClass: "ephemeral" });
    vi.mocked(migrationMod.getJournalKeyCiphertext).mockResolvedValue([]);
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(FAKE_KEYS);

    const { result } = renderHook(() => useMoveTo());
    act(() => { result.current.openDeliveryPending("spawn-dp3"); });
    await act(async () => { result.current.retryDelivery(); });

    expect(result.current.state.phase).toBe("done");
  });

  it("retryDelivery + unenrolled → needs-enroll", async () => {
    vi.mocked(deviceMod.loadDeviceKeys).mockResolvedValue(null);

    const { result } = renderHook(() => useMoveTo());
    act(() => { result.current.openDeliveryPending("spawn-dp4"); });
    await act(async () => { result.current.retryDelivery(); });

    expect(result.current.state.phase).toBe("needs-enroll");
  });
});

describe("MigrateError class", () => {
  it("preserves leg tag on the error instance", () => {
    const e = new MigrateError("boom", "delivery");
    expect(e.leg).toBe("delivery");
    expect(e.message).toBe("boom");
    expect(e instanceof Error).toBe(true);
    expect(e instanceof MigrateError).toBe(true);
  });
});
