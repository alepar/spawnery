/**
 * Tests for enrollment SAS derivation (Phase 5, [WM4][WM5]) and
 * CAS retry-rebase loop ([WM1]).
 *
 * The [WM1] tests exercise approveEnrollment with a FakeASTransport — no
 * live AS required.  The fake transport scripts a real chain built with
 * genuine WebCrypto keys so verifyDeviceSet passes inside the retry loop.
 */

import { describe, it, expect } from "vitest";
import {
  computeEnrollmentSAS,
  generateEnrollmentPayload,
  approveEnrollment,
  serializeEnrollmentPayload,
  deserializeEnrollmentPayload,
  type EnrollmentPayload,
} from "./enrollment";
import {
  buildGenesisEntry,
  buildAddEntry,
  computeEntryHash,
  ConflictError,
  type ASTransport,
  type DeviceSetLog,
  type DeviceRef,
  type OwnerRoot,
  type StoredEntry,
  type AppendResult,
} from "./deviceset";
import { generateDeviceKeys, exportDeviceRef } from "./device";
import { toBase64, fromBase64 } from "./encoding";

const SAS_FORMAT = /^[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$/;

// Fixed chain anchors for hermetic tests (no real chain needed).
const genesisHash = new TextEncoder().encode("genesis-hash-32bytes-padded-xxxx");
const headHash = new TextEncoder().encode("head-hash-32bytes-padded-xxxxxxx");

describe("computeEnrollmentSAS", () => {
  it("returns a correctly formatted SAS code", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const sas = await computeEnrollmentSAS(genesisHash, headHash, payload);
    expect(sas).toMatch(SAS_FORMAT);
  });

  it("produces the same code for the same inputs (enrollee + approver parity)", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const enrolleeSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    const approverSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    expect(enrolleeSAS).toBe(approverSAS);
  });

  /**
   * [WM4] If the enrollee and approver use the same payload bytes but different
   * chain snapshots (e.g. the chain advanced between the link being created and
   * approved), the SAS codes differ — the human notices and must wait for the
   * enrollee to regenerate a fresh link.
   */
  it("[WM4] different head hashes produce different SAS codes", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const staleSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    const freshHead = new TextEncoder().encode("fresh-head-hash-32bytes-padded-x");
    const freshSAS = await computeEnrollmentSAS(genesisHash, freshHead, payload);
    expect(staleSAS).not.toBe(freshSAS);
  });

  /**
   * [WM4] A substituted pubkey in the payload produces a different SAS so a
   * MITM who replaces the new device's pubkeys is detected by the human.
   */
  it("[WM4] substituted payload pubkey produces a different SAS (MITM detection)", async () => {
    const { payload: legitPayload } = await generateEnrollmentPayload("my-browser");
    const { payload: mitmPayload } = await generateEnrollmentPayload("attacker-device");

    // Swap only the x25519 pubkey; keep everything else the same.
    const tamperedPayload: EnrollmentPayload = {
      ...legitPayload,
      x25519Pub: mitmPayload.x25519Pub,
    };

    const legitSAS = await computeEnrollmentSAS(genesisHash, headHash, legitPayload);
    const mitmSAS = await computeEnrollmentSAS(genesisHash, headHash, tamperedPayload);

    expect(mitmSAS).not.toBe(legitSAS);
  });
});

describe("enrollment payload serialization", () => {
  it("round-trips through serialize / deserialize", async () => {
    const { payload } = await generateEnrollmentPayload("round-trip-device");
    const encoded = serializeEnrollmentPayload(payload);
    const decoded = deserializeEnrollmentPayload(encoded);
    expect(decoded.x25519Pub).toBe(payload.x25519Pub);
    expect(decoded.signPub).toBe(payload.signPub);
    expect(decoded.deviceName).toBe(payload.deviceName);
    expect(decoded.expiresAt).toBe(payload.expiresAt);
  });
});

// ── [WM1] Hermetic CAS retry-rebase tests ────────────────────────────────────

/**
 * Builds a minimal valid genesis log with device1 + recovery co-signing.
 * Returns real WebCrypto keys so verifyDeviceSet passes inside retry loops.
 */
async function buildTestGenesisLog(): Promise<{
  device1Keys: Awaited<ReturnType<typeof generateDeviceKeys>>;
  d1DSRef: DeviceRef;
  genesis: StoredEntry;
  genesisLog: DeviceSetLog;
  genesisHead: string;
  ownerRoot: OwnerRoot;
}> {
  const device1Keys = await generateDeviceKeys();
  const recoveryKeys = await generateDeviceKeys();
  const d1Raw = await exportDeviceRef(device1Keys);
  const recRaw = await exportDeviceRef(recoveryKeys);
  const d1DSRef: DeviceRef = {
    x25519_pub: toBase64(d1Raw.x25519Pub),
    sign_pub: toBase64(d1Raw.signPub),
  };
  const recDSRef: DeviceRef = {
    x25519_pub: toBase64(recRaw.x25519Pub),
    sign_pub: toBase64(recRaw.signPub),
  };
  const genesis = await buildGenesisEntry(
    d1DSRef,
    recDSRef,
    "device1",
    "recovery",
    device1Keys.ecdsaPrivate,
    recoveryKeys.ecdsaPrivate,
  );
  const genesisLog: DeviceSetLog = { entries: [genesis] };
  const genesisHead = toBase64(await computeEntryHash(genesis));
  const ownerRoot: OwnerRoot = {
    device1_sign_pub: d1DSRef.sign_pub,
    recovery_sign_pub: recDSRef.sign_pub,
  };
  return { device1Keys, d1DSRef, genesis, genesisLog, genesisHead, ownerRoot };
}

describe("[WM1] approveEnrollment CAS retry-rebase", () => {
  /**
   * Test 1: First append returns CAS-conflict with a new head; client
   * re-fetches/rebases; second append succeeds.  Verifies:
   *   - fetchLog called ≥2 times
   *   - append called exactly 2 times
   *   - result.headVersion equals the post-rebase version
   *   - finally-appended entry.body.prev equals the rebased head hash
   *   - sweepProgress is initialised (not null/undefined)
   */
  it("retries on conflict, rebuilds on new head, and succeeds on second append", async () => {
    const { device1Keys, d1DSRef, genesis, genesisLog, genesisHead, ownerRoot } =
      await buildTestGenesisLog();

    // Build a valid "unrelated advance" entry so the rebased log passes verifyDeviceSet.
    const otherKeys = await generateDeviceKeys();
    const otherRaw = await exportDeviceRef(otherKeys);
    const otherDSRef: DeviceRef = {
      x25519_pub: toBase64(otherRaw.x25519Pub),
      sign_pub: toBase64(otherRaw.signPub),
    };
    const addOther = await buildAddEntry(
      genesisLog,
      otherDSRef,
      "other-device",
      d1DSRef,
      device1Keys.ecdsaPrivate,
    );
    const rebasedLog: DeviceSetLog = { entries: [genesis, addOther] };
    const rebasedHead = toBase64(await computeEntryHash(addOther));

    let fetchLogCallCount = 0;
    let appendCallCount = 0;
    let lastAppendedEntry: StoredEntry | undefined;

    const fake: ASTransport = {
      fetchLog: async () => {
        fetchLogCallCount++;
        if (fetchLogCallCount === 1) {
          return { log: genesisLog, head: genesisHead, version: 1 };
        }
        return { log: rebasedLog, head: rebasedHead, version: 2 };
      },
      append: async (entry: StoredEntry): Promise<AppendResult> => {
        appendCallCount++;
        if (appendCallCount === 1) {
          throw new ConflictError(rebasedHead, 2);
        }
        lastAppendedEntry = entry;
        return { version: 3, head: "final-head-b64" };
      },
    };

    // Build enrollment payload for a new device
    const newKeys = await generateDeviceKeys();
    const newRaw = await exportDeviceRef(newKeys);
    const payload: EnrollmentPayload = {
      x25519Pub: toBase64(newRaw.x25519Pub),
      signPub: toBase64(newRaw.signPub),
      deviceName: "new-browser",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    };

    const result = await approveEnrollment({
      payload,
      transport: fake,
      approverKeys: device1Keys,
      ownerRoot,
      secretIds: [],
    });

    // Retry happened: append called twice, fetchLog at least twice
    expect(appendCallCount).toBe(2);
    expect(fetchLogCallCount).toBeGreaterThanOrEqual(2);

    // Result reflects the successful second append
    expect(result.headVersion).toBe(3);

    // The re-built entry chains onto the rebased head (proves rebuild, not stale prev)
    expect(lastAppendedEntry).toBeDefined();
    const body = JSON.parse(new TextDecoder().decode(fromBase64(lastAppendedEntry!.body)));
    expect(body.prev).toBe(rebasedHead);

    // Sweep was initialised (WM2)
    expect(result.sweepProgress).toBeDefined();
    expect(result.sweepProgress.targetVersion).toBeGreaterThan(0);
  });

  /**
   * Test 2: append always conflicts → give-up after MAX_RETRIES.
   * Verifies append call count == MAX_RETRIES+1 (6), no sweep started.
   */
  it("gives up with ConflictError after MAX_RETRIES failed appends", async () => {
    const { device1Keys, genesis, genesisLog, genesisHead, ownerRoot } =
      await buildTestGenesisLog();

    let appendCallCount = 0;

    const fake: ASTransport = {
      fetchLog: async () => ({ log: genesisLog, head: genesisHead, version: 1 }),
      append: async (): Promise<AppendResult> => {
        appendCallCount++;
        throw new ConflictError(genesisHead, 1);
      },
    };

    // genesis entry is unused here; just need the log to stay valid for rebase verifications
    void genesis;

    const newKeys = await generateDeviceKeys();
    const newRaw = await exportDeviceRef(newKeys);
    const payload: EnrollmentPayload = {
      x25519Pub: toBase64(newRaw.x25519Pub),
      signPub: toBase64(newRaw.signPub),
      deviceName: "new-browser",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    };

    await expect(
      approveEnrollment({
        payload,
        transport: fake,
        approverKeys: device1Keys,
        ownerRoot,
        secretIds: [],
      }),
    ).rejects.toBeInstanceOf(ConflictError);

    // 1 initial + 5 retries = 6 append calls (MAX_RETRIES = 5)
    expect(appendCallCount).toBe(6);
  });
});
