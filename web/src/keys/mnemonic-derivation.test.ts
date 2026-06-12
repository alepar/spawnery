/**
 * Cross-language key derivation test: mnemonic → (X25519 pub, P-256 pub).
 *
 * Reads the Go-generated golden vectors from
 * internal/secrets/seal/testdata/owner-seal/mnemonic_derivation.json and
 * re-derives both keypairs using the TypeScript path, verifying that the
 * public keys match exactly (Go↔TS interop, Phase 3 / Phase 7).
 *
 * This is the vitest side of the requirement 'Go<->TS shared test vectors
 * checked from BOTH vitest and go test' (Phase 7).
 */

import { describe, it, expect } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { deriveDeviceKeysFromMnemonic, exportDeviceRef } from "./device";
import { fromBase64 } from "./encoding";

const TESTDATA_DIR = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/secrets/seal/testdata/owner-seal",
);

interface DerivationVector {
  mnemonic: string;
  x25519_pub: string; // base64 raw 32 bytes
  sign_pub: string;   // base64 SEC1-uncompressed P-256 65 bytes
  x25519_priv: string; // base64 raw 32 bytes (not used here; tested on Go side)
}

function toHex(b: Uint8Array): string {
  return Array.from(b).map((x) => x.toString(16).padStart(2, "0")).join("");
}

describe("mnemonic derivation — Go↔TS cross-language vectors", () => {
  const raw = fs.readFileSync(
    path.join(TESTDATA_DIR, "mnemonic_derivation.json"),
    "utf8",
  );
  const vectors: DerivationVector[] = JSON.parse(raw);

  it("has 3 test vectors", () => {
    expect(vectors).toHaveLength(3);
  });

  for (let i = 0; i < 3; i++) {
    // Use a closure to capture i
    const idx = i;
    it(`vector[${idx}] x25519_pub matches Go derivation`, async () => {
      const v = vectors[idx];
      const keys = await deriveDeviceKeysFromMnemonic(v.mnemonic, "");
      const ref = await exportDeviceRef(keys);
      const expected = fromBase64(v.x25519_pub);
      expect(toHex(ref.x25519Pub)).toBe(toHex(expected));
    });

    it(`vector[${idx}] sign_pub (P-256) matches Go derivation`, async () => {
      const v = vectors[idx];
      const keys = await deriveDeviceKeysFromMnemonic(v.mnemonic, "");
      const ref = await exportDeviceRef(keys);
      const expected = fromBase64(v.sign_pub);
      expect(ref.signPub[0]).toBe(0x04); // SEC1 uncompressed prefix
      expect(toHex(ref.signPub)).toBe(toHex(expected));
    });
  }
});
