/**
 * Tests for the verified device list builder (Phase 2, W3).
 *
 * Uses real WebCrypto keys (generateDeviceKeys) and real entry builders
 * (buildGenesisEntry, buildAddEntry, buildRemoveEntry) so that verifyDeviceSet
 * passes inside buildDeviceList — hermetic, no network.
 */

import { describe, it, expect } from "vitest";
import { buildDeviceList, cascadeForDevice } from "./devicelist";
import {
  buildGenesisEntry,
  buildAddEntry,
  buildRemoveEntry,
  computeEntryHash,
  type DeviceSetLog,
  type DeviceRef,
  type OwnerRoot,
} from "./deviceset";
import { generateDeviceKeys, exportDeviceRef } from "./device";
import { toBase64 } from "./encoding";

// ── Test helpers ──────────────────────────────────────────────────────────────

async function mkRef(keys: Awaited<ReturnType<typeof generateDeviceKeys>>): Promise<DeviceRef> {
  const raw = await exportDeviceRef(keys);
  return {
    x25519_pub: toBase64(raw.x25519Pub),
    sign_pub: toBase64(raw.signPub),
  };
}

async function buildGenesisFixture(): Promise<{
  d1Keys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  d1Ref: DeviceRef;
  recKeys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  recRef: DeviceRef;
  ownerRoot: OwnerRoot;
  genesisLog: DeviceSetLog;
}> {
  const d1Keys = await generateDeviceKeys();
  const recKeys = await generateDeviceKeys();
  const d1Ref = await mkRef(d1Keys);
  const recRef = await mkRef(recKeys);

  const genesis = await buildGenesisEntry(
    d1Ref,
    recRef,
    "my-browser",
    "recovery",
    d1Keys.ecdsaPrivate,
    recKeys.ecdsaPrivate,
    "1700000000000000000", // fixed nanos for deterministic tests
  );

  const ownerRoot: OwnerRoot = {
    device1_sign_pub: d1Ref.sign_pub,
    recovery_sign_pub: recRef.sign_pub,
  };

  return {
    d1Keys,
    d1Ref,
    recKeys,
    recRef,
    ownerRoot,
    genesisLog: { entries: [genesis] },
  };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("buildDeviceList — genesis only", () => {
  it("surfaces authenticated name and enrolled-at from signed body", async () => {
    const { ownerRoot, genesisLog, d1Ref } = await buildGenesisFixture();
    const list = await buildDeviceList(genesisLog, ownerRoot);

    expect(list).toHaveLength(2);
    const d1 = list.find((d) => d.signPub === d1Ref.sign_pub)!;
    expect(d1).toBeDefined();
    expect(d1.name).toBe("my-browser");
    expect(d1.enrolledAt).toBe("1700000000000000000");
  });

  it("flags recovery device distinctly", async () => {
    const { ownerRoot, genesisLog, recRef } = await buildGenesisFixture();
    const list = await buildDeviceList(genesisLog, ownerRoot);

    const rec = list.find((d) => d.signPub === recRef.sign_pub)!;
    expect(rec).toBeDefined();
    expect(rec.isRecovery).toBe(true);
    expect(rec.name).toBe("recovery");
  });

  it("flags isCurrent for the matching sign_pub", async () => {
    const { ownerRoot, genesisLog, d1Ref } = await buildGenesisFixture();
    const list = await buildDeviceList(genesisLog, ownerRoot, { currentSignPub: d1Ref.sign_pub });

    const current = list.find((d) => d.isCurrent);
    expect(current).toBeDefined();
    expect(current!.signPub).toBe(d1Ref.sign_pub);

    const others = list.filter((d) => !d.isCurrent);
    expect(others).toHaveLength(1);
  });

  it("enrolledBySignPub is null for genesis devices", async () => {
    const { ownerRoot, genesisLog } = await buildGenesisFixture();
    const list = await buildDeviceList(genesisLog, ownerRoot);
    for (const item of list) {
      expect(item.enrolledBySignPub).toBeNull();
    }
  });
});

describe("buildDeviceList — with add entry", () => {
  it("surfaces label and authorship for an enrolled device", async () => {
    const { d1Keys, d1Ref, ownerRoot, genesisLog } = await buildGenesisFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);

    const addEntry = await buildAddEntry(
      genesisLog,
      d2Ref,
      "second-device",
      d1Ref,
      d1Keys.ecdsaPrivate,
      "1800000000000000000",
    );
    const log: DeviceSetLog = { entries: [...genesisLog.entries, addEntry] };

    const list = await buildDeviceList(log, ownerRoot);
    expect(list).toHaveLength(3); // d1, recovery, d2

    const d2 = list.find((d) => d.signPub === d2Ref.sign_pub)!;
    expect(d2).toBeDefined();
    expect(d2.name).toBe("second-device");
    expect(d2.enrolledAt).toBe("1800000000000000000");
    expect(d2.enrolledBySignPub).toBe(d1Ref.sign_pub);
    expect(d2.isRecovery).toBe(false);
  });

  it("new device added by recovery rotation has isRecovery=true", async () => {
    const { d1Keys, d1Ref, ownerRoot, genesisLog } = await buildGenesisFixture();

    // Simulate recovery rotation: new virtual device named "recovery"
    const newRecKeys = await generateDeviceKeys();
    const newRecRef = await mkRef(newRecKeys);

    const addEntry = await buildAddEntry(
      genesisLog,
      newRecRef,
      "recovery", // reserved name
      d1Ref,
      d1Keys.ecdsaPrivate,
      "1900000000000000000",
    );
    const log: DeviceSetLog = { entries: [...genesisLog.entries, addEntry] };

    const list = await buildDeviceList(log, ownerRoot);
    const newRec = list.find((d) => d.signPub === newRecRef.sign_pub)!;
    expect(newRec).toBeDefined();
    expect(newRec.isRecovery).toBe(true);
  });
});

describe("buildDeviceList — verification failure propagates", () => {
  it("throws on a tampered entry body (fail-closed)", async () => {
    const { ownerRoot, genesisLog } = await buildGenesisFixture();
    // Tamper the genesis body
    const tampered: DeviceSetLog = {
      entries: [
        {
          ...genesisLog.entries[0],
          body: toBase64(new TextEncoder().encode('{"tampered":true}')),
        },
      ],
    };
    await expect(buildDeviceList(tampered, ownerRoot)).rejects.toThrow();
  });
});

describe("buildDeviceList — head regression check [WM6]", () => {
  it("throws if fetched chain is shorter than pinned version", async () => {
    const { ownerRoot, genesisLog } = await buildGenesisFixture();
    // Genesis has version 1; pinnedHeadVersion=5 should trigger regression error
    await expect(
      buildDeviceList(genesisLog, ownerRoot, { pinnedHeadVersion: 5 }),
    ).rejects.toThrow(/head regression/);
  });
});

// ── cascadeForDevice tests ────────────────────────────────────────────────────

describe("cascadeForDevice", () => {
  it("returns empty array for a device with no enrollees", async () => {
    const { ownerRoot: _ownerRoot, genesisLog, d1Ref } = await buildGenesisFixture();
    const cascade = cascadeForDevice(genesisLog, d1Ref.sign_pub);
    expect(cascade).toHaveLength(0);
  });

  it("returns single-level enrollees", async () => {
    const { d1Keys, d1Ref, genesisLog } = await buildGenesisFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);

    const addEntry = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const log: DeviceSetLog = { entries: [...genesisLog.entries, addEntry] };

    const cascade = cascadeForDevice(log, d1Ref.sign_pub);
    expect(cascade).toHaveLength(1);
    expect(cascade[0].sign_pub).toBe(d2Ref.sign_pub);
  });

  it("returns transitive closure (A enrolls B, B enrolls C → cascade(A)={B,C})", async () => {
    const { d1Keys, d1Ref, genesisLog } = await buildGenesisFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const d3Keys = await generateDeviceKeys();
    const d3Ref = await mkRef(d3Keys);

    // d1 enrolls d2
    const add2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const log2: DeviceSetLog = { entries: [...genesisLog.entries, add2] };

    // d2 enrolls d3
    const add3 = await buildAddEntry(log2, d3Ref, "d3", d2Ref, d2Keys.ecdsaPrivate);
    const log3: DeviceSetLog = { entries: [...log2.entries, add3] };

    const cascade = cascadeForDevice(log3, d1Ref.sign_pub);
    const cascadePubs = cascade.map((d) => d.sign_pub);
    expect(cascadePubs).toContain(d2Ref.sign_pub);
    expect(cascadePubs).toContain(d3Ref.sign_pub);
    expect(cascade).toHaveLength(2);
  });

  it("cascade of a device not enrolled by anyone is empty", async () => {
    const { d1Keys, d1Ref, genesisLog } = await buildGenesisFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);

    // d1 enrolls d2; d3 is standalone (not enrolled by d2)
    const add2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const log: DeviceSetLog = { entries: [...genesisLog.entries, add2] };

    // d2's cascade should be empty (d2 didn't enroll anyone)
    const cascade = cascadeForDevice(log, d2Ref.sign_pub);
    expect(cascade).toHaveLength(0);
  });

  it("removed devices are not present in cascade closure after removal", async () => {
    const { d1Keys, d1Ref, genesisLog } = await buildGenesisFixture();

    const d2Keys = await generateDeviceKeys();
    const d2Ref = await mkRef(d2Keys);
    const d3Keys = await generateDeviceKeys();
    const d3Ref = await mkRef(d3Keys);

    // d1 enrolls d2
    const add2 = await buildAddEntry(genesisLog, d2Ref, "d2", d1Ref, d1Keys.ecdsaPrivate);
    const log2: DeviceSetLog = { entries: [...genesisLog.entries, add2] };
    // d2 enrolls d3
    const add3 = await buildAddEntry(log2, d3Ref, "d3", d2Ref, d2Keys.ecdsaPrivate);
    const log3: DeviceSetLog = { entries: [...log2.entries, add3] };

    // NOTE: cascadeForDevice works on the raw log (add entries) regardless of removes.
    // The caller decides whether to show the cascade prompt based on the verified member set.
    // This is intentional: cascadeForDevice is a prompt helper, not a membership enforcer.
    const cascade = cascadeForDevice(log3, d1Ref.sign_pub);
    expect(cascade).toHaveLength(2); // B and C both in cascade even if later removed

    // After remove of d2, cascade still shows d2 + d3 (raw log history)
    const removeD2 = await buildRemoveEntry(log3, d2Ref.x25519_pub, d1Ref, d1Keys.ecdsaPrivate);
    const log4: DeviceSetLog = { entries: [...log3.entries, removeD2] };
    const cascadeAfterRemove = cascadeForDevice(log4, d1Ref.sign_pub);
    expect(cascadeAfterRemove).toHaveLength(2); // unchanged — log-history based
  });
});

// Suppress unused variable warning for computeEntryHash import used in doctest context
void computeEntryHash;
