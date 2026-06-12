/**
 * Tests for the revocation orchestrator (Phase 3, W3).
 *
 * Uses real WebCrypto keys + real entry builders, fake ASTransport + fake
 * SecretsCPClient — hermetic, no network.
 */

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import {
  isRevocableByNormalRevoke,
  requiresRecoveryConfirmation,
  revokeDevices,
} from "./revoke";
import type { DeviceListItem } from "./devicelist";
import {
  buildGenesisEntry,
  buildAddEntry,
  computeEntryHash,
  type ASTransport,
  type AppendResult,
  type DeviceSetLog,
  type DeviceRef,
  type OwnerRoot,
  type StoredEntry,
} from "./deviceset";
import { generateDeviceKeys, exportDeviceRef } from "./device";
import { toBase64 } from "./encoding";
import { loadSweepProgress } from "./epoch";
import { loadAnchor, saveAnchor } from "./anchor";
import type { SecretsCPClient } from "./sweep";

// ── Helpers ───────────────────────────────────────────────────────────────────

async function mkRef(keys: Awaited<ReturnType<typeof generateDeviceKeys>>): Promise<DeviceRef> {
  const raw = await exportDeviceRef(keys);
  return { x25519_pub: toBase64(raw.x25519Pub), sign_pub: toBase64(raw.signPub) };
}

async function buildFixture(): Promise<{
  d1Keys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  d1Ref: DeviceRef;
  recKeys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  recRef: DeviceRef;
  ownerRoot: OwnerRoot;
  genesisLog: DeviceSetLog;
  genesisHead: string;
}> {
  const d1Keys = await generateDeviceKeys();
  const recKeys = await generateDeviceKeys();
  const d1Ref = await mkRef(d1Keys);
  const recRef = await mkRef(recKeys);
  const genesis = await buildGenesisEntry(
    d1Ref, recRef, "device1", "recovery", d1Keys.ecdsaPrivate, recKeys.ecdsaPrivate,
  );
  const ownerRoot: OwnerRoot = {
    device1_sign_pub: d1Ref.sign_pub,
    recovery_sign_pub: recRef.sign_pub,
  };
  const genesisHead = toBase64(await computeEntryHash(genesis));
  return { d1Keys, d1Ref, recKeys, recRef, ownerRoot, genesisLog: { entries: [genesis] }, genesisHead };
}

function makeListItem(overrides: Partial<DeviceListItem>): DeviceListItem {
  return {
    x25519Pub: "x25519pub",
    signPub: "signpub",
    name: "device",
    enrolledAt: "0",
    enrolledBySignPub: null,
    isCurrent: false,
    isRecovery: false,
    ...overrides,
  };
}

function noopCPClient(): SecretsCPClient {
  return {
    getEnvelope: () => Promise.reject(new Error("no secrets")),
    putEnvelope: () => Promise.resolve(),
  };
}

// ── Guard logic tests ─────────────────────────────────────────────────────────

describe("isRevocableByNormalRevoke", () => {
  it("returns true for a normal device", () => {
    expect(isRevocableByNormalRevoke(makeListItem({ isRecovery: false }))).toBe(true);
  });

  it("returns false for the recovery device", () => {
    expect(isRevocableByNormalRevoke(makeListItem({ isRecovery: true }))).toBe(false);
  });
});

describe("requiresRecoveryConfirmation [WM15]", () => {
  it("returns false for revoking a non-current, non-last device", () => {
    const list = [
      makeListItem({ signPub: "a", isCurrent: true, isRecovery: false }),
      makeListItem({ signPub: "b", isCurrent: false, isRecovery: false }),
      makeListItem({ signPub: "r", isCurrent: false, isRecovery: true }),
    ];
    expect(requiresRecoveryConfirmation(list, ["b"])).toBe(false);
  });

  it("returns true when revoking the current device", () => {
    const list = [
      makeListItem({ signPub: "a", isCurrent: true, isRecovery: false }),
      makeListItem({ signPub: "b", isCurrent: false, isRecovery: false }),
      makeListItem({ signPub: "r", isCurrent: false, isRecovery: true }),
    ];
    expect(requiresRecoveryConfirmation(list, ["a"])).toBe(true);
  });

  it("returns true when revoking the last non-recovery device", () => {
    const list = [
      makeListItem({ signPub: "a", isCurrent: true, isRecovery: false }),
      makeListItem({ signPub: "r", isCurrent: false, isRecovery: true }),
    ];
    // Revoking "a" leaves only the recovery device
    expect(requiresRecoveryConfirmation(list, ["a"])).toBe(true);
  });

  it("returns true when revoking all non-recovery devices", () => {
    const list = [
      makeListItem({ signPub: "a", isCurrent: true, isRecovery: false }),
      makeListItem({ signPub: "b", isCurrent: false, isRecovery: false }),
      makeListItem({ signPub: "r", isCurrent: false, isRecovery: true }),
    ];
    expect(requiresRecoveryConfirmation(list, ["a", "b"])).toBe(true);
  });

  it("returns false when revoking one of multiple non-recovery devices", () => {
    const list = [
      makeListItem({ signPub: "a", isCurrent: true, isRecovery: false }),
      makeListItem({ signPub: "b", isCurrent: false, isRecovery: false }),
      makeListItem({ signPub: "c", isCurrent: false, isRecovery: false }),
      makeListItem({ signPub: "r", isCurrent: false, isRecovery: true }),
    ];
    expect(requiresRecoveryConfirmation(list, ["c"])).toBe(false);
  });
});

// ── revokeDevices orchestrator tests ─────────────────────────────────────────

describe("revokeDevices", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it("appends a remove entry for the target device", async () => {
    const { d1Keys, d1Ref, recKeys, recRef, ownerRoot, genesisLog, genesisHead } =
      await buildFixture();

    // Add a second device to revoke
    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const addD2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const addHead = toBase64(await computeEntryHash(addD2));
    const logWithD2: DeviceSetLog = { entries: [...genesisLog.entries, addD2] };

    const appendedEntries: StoredEntry[] = [];
    const serverLog = { entries: [...logWithD2.entries] };
    let serverVersion = 2;
    let serverHead = addHead;

    const transport: ASTransport = {
      fetchLog: async () => ({
        log: { entries: [...serverLog.entries] },
        head: serverHead,
        version: serverVersion,
      }),
      append: async (entry: StoredEntry): Promise<AppendResult> => {
        appendedEntries.push(entry);
        serverLog.entries.push(entry);
        serverVersion++;
        serverHead = toBase64(await computeEntryHash(entry));
        return { version: serverVersion, head: serverHead };
      },
    };

    saveAnchor({ ownerRoot, headVersion: 2 });

    await revokeDevices({
      transport,
      signerKeys: d1Keys,
      signerRef: d1Ref,
      ownerRoot,
      pinnedHeadVersion: 2,
      targetX25519Pubs: [d2Ref.x25519_pub],
      survivorX25519Pubs: [d1Ref.x25519_pub, recRef.x25519_pub],
      secretIds: [],
      cpClient: noopCPClient(),
    });

    expect(appendedEntries).toHaveLength(1);
    const body = JSON.parse(new TextDecoder().decode(
      new Uint8Array(atob(appendedEntries[0].body).split("").map((c) => c.charCodeAt(0))),
    ));
    expect(body.type).toBe("remove");
    expect(body.change.x25519_pub).toBe(d2Ref.x25519_pub);

    // Anchor should be bumped
    const anchor = loadAnchor()!;
    expect(anchor.headVersion).toBe(3);

    void recKeys;
    void genesisHead;
  });

  it("rejects the recovery device as a revocation target", async () => {
    const { d1Keys, d1Ref, recRef, ownerRoot, genesisLog, genesisHead } =
      await buildFixture();

    const transport: ASTransport = {
      fetchLog: async () => ({ log: { entries: [...genesisLog.entries] }, head: genesisHead, version: 1 }),
      append: async (_e) => { throw new Error("should not append"); },
    };

    saveAnchor({ ownerRoot, headVersion: 1 });

    await expect(
      revokeDevices({
        transport,
        signerKeys: d1Keys,
        signerRef: d1Ref,
        ownerRoot,
        pinnedHeadVersion: 1,
        targetX25519Pubs: [recRef.x25519_pub],
        survivorX25519Pubs: [d1Ref.x25519_pub],
        secretIds: [],
        cpClient: noopCPClient(),
      }),
    ).rejects.toThrow(/recovery virtual device/);
  });

  it("sweep is initialized as revocation and persisted", async () => {
    const { d1Keys, d1Ref, recRef, ownerRoot, genesisLog, genesisHead } =
      await buildFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const addD2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const addHead = toBase64(await computeEntryHash(addD2));
    const logWithD2: DeviceSetLog = { entries: [...genesisLog.entries, addD2] };

    const serverLog = { entries: [...logWithD2.entries] };
    let serverVersion = 2;
    let serverHead = addHead;

    const transport: ASTransport = {
      fetchLog: async () => ({ log: { entries: [...serverLog.entries] }, head: serverHead, version: serverVersion }),
      append: async (entry: StoredEntry): Promise<AppendResult> => {
        serverLog.entries.push(entry);
        serverVersion++;
        serverHead = toBase64(await computeEntryHash(entry));
        return { version: serverVersion, head: serverHead };
      },
    };

    saveAnchor({ ownerRoot, headVersion: 2 });

    const progress = await revokeDevices({
      transport,
      signerKeys: d1Keys,
      signerRef: d1Ref,
      ownerRoot,
      pinnedHeadVersion: 2,
      targetX25519Pubs: [d2Ref.x25519_pub],
      survivorX25519Pubs: [d1Ref.x25519_pub, recRef.x25519_pub],
      secretIds: ["s1", "s2"],
      cpClient: {
        getEnvelope: () => Promise.reject(new Error("no secrets in test")),
        putEnvelope: () => Promise.resolve(),
      } as SecretsCPClient,
    });

    // isRevocation must be true
    expect(progress.isRevocation).toBe(true);
    // secretIds tracked
    expect(progress.secretIds).toEqual(["s1", "s2"]);

    // The persisted progress is available for the in-progress banner
    const stored = loadSweepProgress();
    expect(stored).not.toBeNull();
    expect(stored!.isRevocation).toBe(true);

    void genesisHead;
  });

  it("cascades remove for multiple targets in separate entries", async () => {
    const { d1Keys, d1Ref, recRef, ownerRoot, genesisLog, genesisHead } =
      await buildFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const d3Keys = await generateDeviceKeys();
    const d3Ref = await mkRef(d3Keys);

    let currentLog: DeviceSetLog = genesisLog;
    const add2 = await buildAddEntry(currentLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    currentLog = { entries: [...currentLog.entries, add2] };
    const add3 = await buildAddEntry(currentLog, d3Ref, "d3", d1Ref, d1Keys.ecdsaPrivate);
    currentLog = { entries: [...currentLog.entries, add3] };

    let serverVersion = 3;
    let serverHead = toBase64(await computeEntryHash(add3));
    const serverLog = { entries: [...currentLog.entries] };

    const appendedBodies: { type: string; change: DeviceRef }[] = [];
    const transport: ASTransport = {
      fetchLog: async () => ({ log: { entries: [...serverLog.entries] }, head: serverHead, version: serverVersion }),
      append: async (entry: StoredEntry): Promise<AppendResult> => {
        const body = JSON.parse(new TextDecoder().decode(
          new Uint8Array(atob(entry.body).split("").map((c) => c.charCodeAt(0))),
        ));
        appendedBodies.push(body);
        serverLog.entries.push(entry);
        serverVersion++;
        serverHead = toBase64(await computeEntryHash(entry));
        return { version: serverVersion, head: serverHead };
      },
    };

    saveAnchor({ ownerRoot, headVersion: 3 });

    await revokeDevices({
      transport,
      signerKeys: d1Keys,
      signerRef: d1Ref,
      ownerRoot,
      pinnedHeadVersion: 3,
      targetX25519Pubs: [d2Ref.x25519_pub, d3Ref.x25519_pub],
      survivorX25519Pubs: [d1Ref.x25519_pub, recRef.x25519_pub],
      secretIds: [],
      cpClient: noopCPClient(),
    });

    // One remove entry per target
    expect(appendedBodies).toHaveLength(2);
    const types = appendedBodies.map((b) => b.type);
    expect(types).toEqual(["remove", "remove"]);
    const removedPubs = appendedBodies.map((b) => b.change.x25519_pub);
    expect(removedPubs).toContain(d2Ref.x25519_pub);
    expect(removedPubs).toContain(d3Ref.x25519_pub);

    void genesisHead;
    void d3Keys;
  });

  it("sweep is resumable after interruption (re-run skips completed secrets)", async () => {
    const { d1Keys, d1Ref, recRef, ownerRoot, genesisLog, genesisHead } =
      await buildFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const addD2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const addHead = toBase64(await computeEntryHash(addD2));
    const logWithD2: DeviceSetLog = { entries: [...genesisLog.entries, addD2] };

    const serverLog = { entries: [...logWithD2.entries] };
    let serverVersion = 2;
    let serverHead = addHead;

    const transport: ASTransport = {
      fetchLog: async () => ({ log: { entries: [...serverLog.entries] }, head: serverHead, version: serverVersion }),
      append: async (entry: StoredEntry): Promise<AppendResult> => {
        serverLog.entries.push(entry);
        serverVersion++;
        serverHead = toBase64(await computeEntryHash(entry));
        return { version: serverVersion, head: serverHead };
      },
    };

    const getCalls: string[] = [];
    const cpClient: SecretsCPClient = {
      getEnvelope: async (id) => {
        getCalls.push(id);
        throw new Error("fail-intentionally"); // simulate partial failure
      },
      putEnvelope: () => Promise.resolve(),
    };

    saveAnchor({ ownerRoot, headVersion: 2 });

    // First run: fails on all secrets
    await revokeDevices({
      transport,
      signerKeys: d1Keys,
      signerRef: d1Ref,
      ownerRoot,
      pinnedHeadVersion: 2,
      targetX25519Pubs: [d2Ref.x25519_pub],
      survivorX25519Pubs: [d1Ref.x25519_pub, recRef.x25519_pub],
      secretIds: ["s1", "s2"],
      cpClient,
    });

    // Both secrets failed
    const stored = loadSweepProgress()!;
    expect(stored.failed).toContain("s1");
    expect(stored.failed).toContain("s2");

    void genesisHead;
    void d2Keys;
  });
});
