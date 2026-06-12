/**
 * Cross-language vector validation for the owner-sealed-secrets primitives.
 *
 * Reads testdata/owner-seal/{mnemonic_derivation,deviceset_chain}.json — the
 * same golden files verified by go test ./... — and validates:
 *   1. mnemonic_derivation.json: structural integrity (base64 lengths, format).
 *   2. deviceset_chain.json: the encodeFields+SHA-256 head-hash algorithm matches
 *      the stored head_hash for every chain entry.
 *
 * Key derivation re-validation (HKDF, X25519, P-256) is part of Phase 3
 * (WebCrypto layer) and will be added when that layer is implemented.
 */
import { describe, it, expect } from "vitest";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Path to the Go testdata directory (relative to this file).
const TESTDATA_DIR = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../internal/secrets/seal/testdata/owner-seal",
);

// ── Types matching the Go JSON shapes ────────────────────────────────────────

interface DerivationVector {
  mnemonic: string;
  x25519_pub: string; // base64 raw 32 bytes
  sign_pub: string; // base64 SEC1-uncompressed 65 bytes
  x25519_priv: string; // base64 raw 32 bytes
}

interface Signature {
  signer_pub: string; // base64 SEC1-uncompressed P-256 pubkey
  sig: string; // base64 ASN.1-DER ECDSA signature
}

interface StoredEntry {
  body: string; // base64 JSON body bytes
  sigs: Signature[];
}

interface EntryVector {
  version: number;
  entry_bytes: string; // base64 json.Marshal(StoredEntry)
  head_hash: string; // base64 sha256(encodeFields(body, sig0.signer_pub, sig0.sig, …))
}

interface ChainVector {
  owner_root: {
    device1_sign_pub: string; // base64
    recovery_sign_pub: string; // base64
  };
  entries: EntryVector[];
}

// ── encodeFields — length-prefixed concatenation (must match seal.go) ────────
//
// Each part is prefixed with an 8-byte big-endian uint64 length.
// sha256(encodeFields(Body, sig0.SignerPub, sig0.Sig, …)) = head_hash.

function encodeFields(...parts: Buffer[]): Buffer {
  const totalLen = parts.reduce((n, p) => n + 8 + p.length, 0);
  const out = Buffer.alloc(totalLen);
  let offset = 0;
  for (const p of parts) {
    out.writeBigUInt64BE(BigInt(p.length), offset);
    offset += 8;
    p.copy(out, offset);
    offset += p.length;
  }
  return out;
}

function computeHeadHash(entry: StoredEntry): string {
  const body = Buffer.from(entry.body, "base64");
  const parts: Buffer[] = [body];
  for (const s of entry.sigs) {
    parts.push(Buffer.from(s.signer_pub, "base64"));
    parts.push(Buffer.from(s.sig, "base64"));
  }
  return createHash("sha256").update(encodeFields(...parts)).digest("base64");
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("seal-vectors mnemonic_derivation.json", () => {
  const raw = fs.readFileSync(
    path.join(TESTDATA_DIR, "mnemonic_derivation.json"),
    "utf8",
  );
  const vectors: DerivationVector[] = JSON.parse(raw);

  it("has exactly 3 entries", () => {
    expect(vectors).toHaveLength(3);
  });

  it("each entry has a non-empty mnemonic", () => {
    for (const v of vectors) {
      expect(v.mnemonic.trim().length).toBeGreaterThan(0);
    }
  });

  it("x25519_pub decodes to 32 bytes", () => {
    for (const v of vectors) {
      const b = Buffer.from(v.x25519_pub, "base64");
      expect(b).toHaveLength(32);
    }
  });

  it("sign_pub decodes to 65 bytes (SEC1 uncompressed P-256)", () => {
    for (const v of vectors) {
      const b = Buffer.from(v.sign_pub, "base64");
      // SEC1 uncompressed P-256: 0x04 || 32-byte X || 32-byte Y = 65 bytes
      expect(b).toHaveLength(65);
      expect(b[0]).toBe(0x04);
    }
  });

  it("x25519_priv decodes to 32 bytes", () => {
    for (const v of vectors) {
      const b = Buffer.from(v.x25519_priv, "base64");
      expect(b).toHaveLength(32);
    }
  });

  it("all mnemonics are unique", () => {
    const mnemonics = vectors.map((v) => v.mnemonic);
    expect(new Set(mnemonics).size).toBe(mnemonics.length);
  });

  it("all x25519_pub values are unique", () => {
    const pubs = vectors.map((v) => v.x25519_pub);
    expect(new Set(pubs).size).toBe(pubs.length);
  });
});

describe("seal-vectors deviceset_chain.json", () => {
  const raw = fs.readFileSync(
    path.join(TESTDATA_DIR, "deviceset_chain.json"),
    "utf8",
  );
  const cv: ChainVector = JSON.parse(raw);

  it("has exactly 3 entries (genesis + add + remove)", () => {
    expect(cv.entries).toHaveLength(3);
  });

  it("entry versions are 1, 2, 3 (monotonic)", () => {
    const versions = cv.entries.map((e) => e.version);
    expect(versions).toEqual([1, 2, 3]);
  });

  it("owner_root has two distinct base64 sign_pub values (65 bytes each)", () => {
    const d1 = Buffer.from(cv.owner_root.device1_sign_pub, "base64");
    const rec = Buffer.from(cv.owner_root.recovery_sign_pub, "base64");
    expect(d1).toHaveLength(65);
    expect(rec).toHaveLength(65);
    expect(d1.equals(rec)).toBe(false);
  });

  it("head_hash matches encodeFields+SHA-256 for each entry", () => {
    for (const ev of cv.entries) {
      const entryJson = Buffer.from(ev.entry_bytes, "base64").toString("utf8");
      const entry: StoredEntry = JSON.parse(entryJson);

      const computed = computeHeadHash(entry);
      expect(computed).toBe(ev.head_hash);
    }
  });

  it("each entry_bytes decodes to a StoredEntry with body and sigs", () => {
    for (const ev of cv.entries) {
      const raw = Buffer.from(ev.entry_bytes, "base64").toString("utf8");
      const entry: StoredEntry = JSON.parse(raw);
      expect(typeof entry.body).toBe("string");
      expect(Array.isArray(entry.sigs)).toBe(true);
      expect(entry.sigs.length).toBeGreaterThan(0);
    }
  });

  it("genesis entry (v1) has two signatures (device1 + recovery co-sign)", () => {
    const genesisRaw = Buffer.from(cv.entries[0].entry_bytes, "base64").toString("utf8");
    const genesis: StoredEntry = JSON.parse(genesisRaw);
    expect(genesis.sigs).toHaveLength(2);
  });

  it("add and remove entries have one signature each (member sign)", () => {
    for (const ev of cv.entries.slice(1)) {
      const raw = Buffer.from(ev.entry_bytes, "base64").toString("utf8");
      const entry: StoredEntry = JSON.parse(raw);
      expect(entry.sigs).toHaveLength(1);
    }
  });

  it("genesis body contains label and recovery_label fields (WM15)", () => {
    const genesisRaw = Buffer.from(cv.entries[0].entry_bytes, "base64").toString("utf8");
    const genesis: StoredEntry = JSON.parse(genesisRaw);
    const body = JSON.parse(Buffer.from(genesis.body, "base64").toString("utf8"));
    expect(body.label).toBeDefined();
    expect(typeof body.label.name).toBe("string");
    expect(typeof body.label.enrolled_at).toBe("string");
    expect(body.recovery_label).toBeDefined();
    expect(typeof body.recovery_label.name).toBe("string");
    expect(typeof body.recovery_label.enrolled_at).toBe("string");
  });

  it("enrolled_at is a decimal string of digits only (WM10 — no float encoding)", () => {
    for (const ev of cv.entries) {
      const raw = Buffer.from(ev.entry_bytes, "base64").toString("utf8");
      const entry: StoredEntry = JSON.parse(raw);
      const body = JSON.parse(Buffer.from(entry.body, "base64").toString("utf8"));
      if (body.label) {
        expect(body.label.enrolled_at).toMatch(/^\d+$/);
        expect(body.label.enrolled_at.includes(".")).toBe(false);
      }
      if (body.recovery_label) {
        expect(body.recovery_label.enrolled_at).toMatch(/^\d+$/);
      }
    }
  });

  it("encodeFields produces the correct binary structure", () => {
    // Unit test for the encodeFields helper itself: one part of length N should
    // produce an 8-byte big-endian N followed by N bytes.
    const p = Buffer.from("hello");
    const encoded = encodeFields(p);
    expect(encoded).toHaveLength(8 + 5);
    // First 8 bytes: big-endian uint64(5)
    expect(encoded.readBigUInt64BE(0)).toBe(BigInt(5));
    // Remaining 5 bytes: the data
    expect(encoded.subarray(8).toString()).toBe("hello");
  });
});
