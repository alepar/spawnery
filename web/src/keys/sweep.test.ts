/**
 * Tests for the re-seal sweep executor ([WM2]).
 */

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { dhkemDerivePrivateScalar, sealEnvelope, openEnvelope, type Envelope } from "./hpke";
import { initSweep, isSweepComplete } from "./epoch";
import { executeSweep, type SecretsCPClient } from "./sweep";

// ── Crypto helpers ────────────────────────────────────────────────────────────

function fromHex(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}

async function importX25519Scalar(scalar: Uint8Array, extractable: boolean): Promise<CryptoKey> {
  const prefix = new Uint8Array([
    0x30, 0x2e, 0x02, 0x01, 0x00,
    0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e,
    0x04, 0x22, 0x04, 0x20,
  ]);
  const pkcs8 = new Uint8Array(prefix.length + 32);
  pkcs8.set(prefix);
  pkcs8.set(scalar, prefix.length);
  return crypto.subtle.importKey("pkcs8", pkcs8, { name: "X25519" }, extractable, ["deriveBits"]);
}

async function x25519PublicBytes(priv: CryptoKey): Promise<Uint8Array> {
  const jwk = await crypto.subtle.exportKey("jwk", priv);
  if (!jwk.x) throw new Error("no x in JWK");
  const b64 = jwk.x.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// ── Fake CP client ────────────────────────────────────────────────────────────

/** fakeCPClient is an in-memory SecretsCPClient for testing. */
function fakeCPClient(store: Map<string, Envelope>): SecretsCPClient {
  return {
    listSecretIdsForSweep: async () => Array.from(store.keys()),
    getEnvelope: async (id) => {
      const env = store.get(id);
      if (!env) throw new Error(`secret not found: ${id}`);
      return env;
    },
    putEnvelope: async (id, env) => {
      store.set(id, env);
    },
  };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("executeSweep [WM2]", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it("re-seals all secrets and marks sweep complete", async () => {
    // Recipient 1: the "existing" device (can open old envelopes)
    const ikmR1 = "6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037";
    const scalar1 = await dhkemDerivePrivateScalar(fromHex(ikmR1));
    const priv1 = await importX25519Scalar(scalar1, false);
    const pub1E = await importX25519Scalar(scalar1, true);
    const pub1 = await x25519PublicBytes(pub1E);

    // Recipient 2: the newly-added device
    const ikmR2 = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef";
    const scalar2 = await dhkemDerivePrivateScalar(fromHex(ikmR2));
    const priv2 = await importX25519Scalar(scalar2, false);
    const pub2E = await importX25519Scalar(scalar2, true);
    const pub2 = await x25519PublicBytes(pub2E);

    // Seal two secrets to recipient 1 only
    const atRest1 = { account_id: "acct", secret_id: "s1", version: 1 };
    const atRest2 = { account_id: "acct", secret_id: "s2", version: 1 };
    const env1 = await sealEnvelope(new TextEncoder().encode("secret-1"), [pub1], atRest1);
    const env2 = await sealEnvelope(new TextEncoder().encode("secret-2"), [pub1], atRest2);

    const store = new Map<string, Envelope>([
      ["s1", env1],
      ["s2", env2],
    ]);
    const cpClient = fakeCPClient(store);

    // Device 1 is the sweeping device; new set includes both devices
    // We need a DeviceKeys-like object; generate fresh keys for the test device
    // but override x25519Private/x25519Public with the known priv1/pub1 material.
    // Since generateDeviceKeys creates non-extractable keys, derive them properly.
    const deviceKeys = {
      x25519Private: priv1,
      // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
      x25519Public: await crypto.subtle.importKey("raw", pub1 as unknown as Uint8Array<ArrayBuffer>, { name: "X25519" }, true, []),
      // ECDSA keys — not used by the sweep executor, generate throw-away keys
      ecdsaPrivate: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).privateKey,
      ecdsaPublic: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).publicKey,
    };
    // Override exportDeviceRef behaviour by injecting pub1 directly
    Object.defineProperty(deviceKeys.x25519Public, "usages", { value: [] });

    const progress = initSweep({
      targetVersion: 2,
      secretIds: ["s1", "s2"],
      isRevocation: false,
    });

    const final = await executeSweep({
      progress,
      deviceKeys: deviceKeys as Parameters<typeof executeSweep>[0]["deviceKeys"],
      newMemberPubs: [pub1, pub2],
      cpClient,
    });

    expect(isSweepComplete(final)).toBe(true);
    expect(final.completed).toContain("s1");
    expect(final.completed).toContain("s2");
    expect(final.failed).toHaveLength(0);

    // Both recipients can now open the re-sealed envelopes
    const resealedS1 = store.get("s1")!;
    const resealedS2 = store.get("s2")!;

    // Version bumped
    expect(resealedS1.at_rest.version).toBe(2);
    expect(resealedS2.at_rest.version).toBe(2);

    // Recipient 2 can open
    const p1 = await openEnvelope(resealedS1, priv2, pub2);
    const p2 = await openEnvelope(resealedS2, priv2, pub2);
    expect(new TextDecoder().decode(p1)).toBe("secret-1");
    expect(new TextDecoder().decode(p2)).toBe("secret-2");
  });

  it("skips already-completed secrets on resume", async () => {
    const ikmR = "6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037";
    const scalar = await dhkemDerivePrivateScalar(fromHex(ikmR));
    const priv = await importX25519Scalar(scalar, false);
    const pubE = await importX25519Scalar(scalar, true);
    const pub = await x25519PublicBytes(pubE);

    const atRest = { account_id: "acct", secret_id: "s1", version: 1 };
    const env = await sealEnvelope(new TextEncoder().encode("val"), [pub], atRest);
    const store = new Map<string, Envelope>([["s1", env]]);

    const getCalls: string[] = [];
    const cpClient: SecretsCPClient = {
      listSecretIdsForSweep: async () => ["s1", "s2"],
      getEnvelope: async (id) => {
        getCalls.push(id);
        return store.get(id)!;
      },
      putEnvelope: async (id, e) => { store.set(id, e); },
    };

    // Simulate a progress state where s1 is already completed
    const progress = initSweep({ targetVersion: 1, secretIds: ["s1", "s2"], isRevocation: false });
    // Manually inject s1 as completed (simulates a prior partial run)
    const resumeProgress = {
      ...progress,
      done: 1,
      completed: ["s1"],
    };

    // s2 doesn't exist in the store → will fail
    const final = await executeSweep({
      progress: resumeProgress,
      deviceKeys: {
        x25519Private: priv,
        // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
        x25519Public: await crypto.subtle.importKey("raw", pub as unknown as Uint8Array<ArrayBuffer>, { name: "X25519" }, true, []),
        ecdsaPrivate: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).privateKey,
        ecdsaPublic: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).publicKey,
      } as Parameters<typeof executeSweep>[0]["deviceKeys"],
      newMemberPubs: [pub],
      cpClient,
    });

    // s1 must NOT have been fetched again (already completed)
    expect(getCalls).not.toContain("s1");
    // s2 failed (not in store)
    expect(final.failed).toContain("s2");
  });

  it("calls onProgress callback for each secret", async () => {
    const ikmR = "6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037";
    const scalar = await dhkemDerivePrivateScalar(fromHex(ikmR));
    const priv = await importX25519Scalar(scalar, false);
    const pubE = await importX25519Scalar(scalar, true);
    const pub = await x25519PublicBytes(pubE);

    const store = new Map<string, Envelope>([
      ["s1", await sealEnvelope(new TextEncoder().encode("v1"), [pub], { account_id: "a", secret_id: "s1", version: 1 })],
      ["s2", await sealEnvelope(new TextEncoder().encode("v2"), [pub], { account_id: "a", secret_id: "s2", version: 1 })],
    ]);

    const progressSnapshots: number[] = [];
    const progress = initSweep({ targetVersion: 1, secretIds: ["s1", "s2"], isRevocation: false });

    await executeSweep({
      progress,
      deviceKeys: {
        x25519Private: priv,
        // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
        x25519Public: await crypto.subtle.importKey("raw", pub as unknown as Uint8Array<ArrayBuffer>, { name: "X25519" }, true, []),
        ecdsaPrivate: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).privateKey,
        ecdsaPublic: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).publicKey,
      } as Parameters<typeof executeSweep>[0]["deviceKeys"],
      newMemberPubs: [pub],
      cpClient: fakeCPClient(store),
      onProgress: (p) => progressSnapshots.push(p.done),
    });

    expect(progressSnapshots).toEqual([1, 2]);
  });

  it("stores the resealed envelope with progress.targetVersion as the new deviceset epoch", async () => {
    const ikmR = "6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037";
    const scalar = await dhkemDerivePrivateScalar(fromHex(ikmR));
    const priv = await importX25519Scalar(scalar, false);
    const pubE = await importX25519Scalar(scalar, true);
    const pub = await x25519PublicBytes(pubE);

    const env = await sealEnvelope(new TextEncoder().encode("v1"), [pub], { account_id: "a", secret_id: "s1", version: 1 });
    const putCalls: Array<{ id: string; epoch: number }> = [];
    const cpClient: SecretsCPClient = {
      listSecretIdsForSweep: async () => ["s1"],
      getEnvelope: async () => env,
      putEnvelope: async (id, _env, devicesetEpoch) => {
        putCalls.push({ id, epoch: devicesetEpoch });
      },
    };

    await executeSweep({
      progress: initSweep({ targetVersion: 9, secretIds: ["s1"], isRevocation: true }),
      deviceKeys: {
        x25519Private: priv,
        x25519Public: await crypto.subtle.importKey("raw", pub as unknown as Uint8Array<ArrayBuffer>, { name: "X25519" }, true, []),
        ecdsaPrivate: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).privateKey,
        ecdsaPublic: (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]) as CryptoKeyPair).publicKey,
      } as Parameters<typeof executeSweep>[0]["deviceKeys"],
      newMemberPubs: [pub],
      cpClient,
    });

    expect(putCalls).toEqual([{ id: "s1", epoch: 9 }]);
  });
});
