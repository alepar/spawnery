/**
 * Tests for session keypair lifecycle.
 *
 * Uses MemoryKeyStore so no real IndexedDB is touched (Node structured-clone of
 * non-extractable CryptoKey is unreliable).
 */

import { describe, it, expect, vi } from "vitest";
import { MemoryKeyStore } from "./keystore";
import {
  getOrCreateSessionKey,
  exportSpkiDer,
  sessionKeyHash,
  keyCanSign,
  signP1363,
  clearSessionKey,
} from "./keypair";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const VECTORS_PATH = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/intent/testdata/intent_vectors.json",
);

function bytesToHex(bytes: Uint8Array): string {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, "0")).join("");
}

function hexToBytes(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < hex.length; i += 2) out[i / 2] = parseInt(hex.slice(i, i + 2), 16);
  return out;
}

describe("getOrCreateSessionKey", () => {
  it("creates a fresh keypair when store is empty", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    expect(kp.privateKey).toBeTruthy();
    expect(kp.publicKey).toBeTruthy();
    expect(kp.privateKey.extractable).toBe(false);
    // Should be persisted
    const loaded = await store.get();
    expect(loaded).not.toBeNull();
  });

  it("returns the same keypair on second call (idempotent)", async () => {
    const store = new MemoryKeyStore();
    const kp1 = await getOrCreateSessionKey(store);
    const kp2 = await getOrCreateSessionKey(store);
    // Same CryptoKey objects (not re-generated)
    expect(kp1.privateKey).toBe(kp2.privateKey);
  });

  it("calls navigator.storage.persist() at creation", async () => {
    const store = new MemoryKeyStore();
    const persistFn = vi.fn().mockResolvedValue(true);
    await getOrCreateSessionKey(store, { persist: persistFn });
    expect(persistFn).toHaveBeenCalledOnce();
  });

  it("does NOT call persist() if key already exists", async () => {
    const store = new MemoryKeyStore();
    const persistFn = vi.fn().mockResolvedValue(true);
    // First call creates
    await getOrCreateSessionKey(store, { persist: persistFn });
    persistFn.mockClear();
    // Second call finds existing key
    await getOrCreateSessionKey(store, { persist: persistFn });
    expect(persistFn).not.toHaveBeenCalled();
  });
});

describe("exportSpkiDer + sessionKeyHash", () => {
  it("SPKI hash matches golden vector", async () => {
    const raw = fs.readFileSync(VECTORS_PATH, "utf8");
    const vectors = JSON.parse(raw) as { spki_der_hex: string; spki_hash_hex: string };

    // Import the golden SPKI public key
    const spkiDer = hexToBytes(vectors.spki_der_hex);
    const pub = await crypto.subtle.importKey(
      "spki",
      spkiDer as unknown as Uint8Array<ArrayBuffer>,
      { name: "ECDSA", namedCurve: "P-256" },
      true,
      ["verify"],
    );

    // Export + hash should match vector
    const exported = await exportSpkiDer(pub);
    expect(bytesToHex(exported)).toBe(vectors.spki_der_hex);

    const hash = await sessionKeyHash(exported);
    expect(bytesToHex(hash)).toBe(vectors.spki_hash_hex);
  });
});

describe("keyCanSign", () => {
  it("returns true for a live key", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    expect(await keyCanSign(kp.privateKey)).toBe(true);
  });
});

describe("signP1363", () => {
  it("produces a 64-byte P1363 signature that verifies", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const msg = new TextEncoder().encode("test message");
    const sig = await signP1363(kp.privateKey, msg);
    expect(sig.length).toBe(64);

    // Verify with WebCrypto
    const ok = await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      kp.publicKey,
      sig as unknown as Uint8Array<ArrayBuffer>,
      msg as unknown as Uint8Array<ArrayBuffer>,
    );
    expect(ok).toBe(true);
  });
});

describe("clearSessionKey", () => {
  it("removes the key from the store", async () => {
    const store = new MemoryKeyStore();
    await getOrCreateSessionKey(store);
    await clearSessionKey(store);
    expect(await store.get()).toBeNull();
  });
});
