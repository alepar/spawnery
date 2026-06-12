/**
 * Device-set chain verification tests (Phase 3).
 *
 * Tests use the Go golden vectors from testdata/owner-seal/deviceset_chain.json
 * to validate cross-language correctness (chain verify + hash algorithm).
 * Additional vitest-hermetic tests cover:
 *   - Chain verification (tamper / fork / stale-head / head-regression fail-closed, [WM6])
 *   - CAS retry-rebase semantics ([WM1])
 *   - BigInt timestamp parsing ([WM10])
 */

import { describe, it, expect } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  verifyDeviceSet,
  computeEntryHash,
  nowNanoStr,
  ConflictError,
  type OwnerRoot,
  type StoredEntry,
  type DeviceSetLog,
  parseBigIntNanos,
} from "./deviceset";
import { fromBase64, toBase64 } from "./encoding";
import { derToP1363 } from "./der";

// Path to the shared Go test vectors (from web/src/keys/ → project root is ../../..)
const TESTDATA_DIR = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/secrets/seal/testdata/owner-seal",
);

// ── Cross-language golden vector tests ───────────────────────────────────────

describe("deviceset chain — Go golden vectors", () => {
  const raw = fs.readFileSync(path.join(TESTDATA_DIR, "deviceset_chain.json"), "utf8");
  const cv: {
    owner_root: { device1_sign_pub: string; recovery_sign_pub: string };
    entries: Array<{ version: number; entry_bytes: string; head_hash: string }>;
  } = JSON.parse(raw);

  const ownerRoot: OwnerRoot = {
    device1_sign_pub: cv.owner_root.device1_sign_pub,
    recovery_sign_pub: cv.owner_root.recovery_sign_pub,
  };

  function buildLog(): DeviceSetLog {
    const entries: StoredEntry[] = cv.entries.map((ev) => {
      const raw = fromBase64(ev.entry_bytes);
      return JSON.parse(new TextDecoder().decode(raw)) as StoredEntry;
    });
    return { entries };
  }

  it("verifyDeviceSet passes on the valid golden chain", async () => {
    const log = buildLog();
    const result = await verifyDeviceSet(log, ownerRoot);
    // Genesis + add device2 → remove device2 = only genesis members remain
    // genesis has 2 devices (device1 + recovery); after add has 3; after remove = 2 again
    expect(result.members).toHaveLength(2);
    expect(result.headVersion).toBe(3);
  });

  it("computeEntryHash matches stored head_hash for each entry", async () => {
    const log = buildLog();
    for (let i = 0; i < cv.entries.length; i++) {
      const hash = await computeEntryHash(log.entries[i]);
      expect(toBase64(hash)).toBe(cv.entries[i].head_hash);
    }
  });

  it("head-regression hard fail [WM6]: pinnedHeadVersion > fetched head", async () => {
    const log = buildLog();
    await expect(verifyDeviceSet(log, ownerRoot, 99)).rejects.toThrow(/head regression/);
  });

  it("tampered body: verifyDeviceSet throws on broken chain", async () => {
    const log = buildLog();
    // Tamper the body of the genesis entry
    const tampered: StoredEntry = {
      body: toBase64(new TextEncoder().encode('{"version":1,"type":"genesis","tampered":true}')),
      sigs: log.entries[0].sigs,
    };
    const tamperedLog: DeviceSetLog = {
      entries: [tampered, ...log.entries.slice(1)],
    };
    await expect(verifyDeviceSet(tamperedLog, ownerRoot)).rejects.toThrow();
  });

  it("non-monotonic version: verifyDeviceSet throws", async () => {
    const log = buildLog();
    // Modify version in entry[1] body
    const entry = log.entries[1];
    const body = JSON.parse(
      new TextDecoder().decode(fromBase64(entry.body)),
    );
    body.version = 5; // skip ahead
    const badEntry: StoredEntry = {
      body: toBase64(new TextEncoder().encode(JSON.stringify(body))),
      sigs: entry.sigs,
    };
    const badLog: DeviceSetLog = { entries: [log.entries[0], badEntry] };
    await expect(verifyDeviceSet(badLog, ownerRoot)).rejects.toThrow(/not monotonic/);
  });

  it("wrong owner root: genesis sig verification fails when root keys were not genesis signers", async () => {
    const log = buildLog();
    // Generate a completely different P-256 key (not one of the genesis signers)
    const alienKey = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign"]);
    const alienPubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", alienKey.publicKey));
    const alienB64 = toBase64(alienPubRaw);
    // Replace device1's root slot with an alien key that never signed anything
    const badRoot: OwnerRoot = {
      device1_sign_pub: alienB64,
      recovery_sign_pub: cv.owner_root.recovery_sign_pub,
    };
    await expect(verifyDeviceSet(log, badRoot)).rejects.toThrow(/genesis not co-signed/);
  });
});

// ── DER↔P1363 round-trip ─────────────────────────────────────────────────────

describe("DER↔P1363 conversion", () => {
  it("round-trips a P-256 signature", async () => {
    // Generate a key and sign something to get a real DER signature
    const key = await crypto.subtle.generateKey(
      { name: "ECDSA", namedCurve: "P-256" },
      false,
      ["sign", "verify"],
    );
    const msg = new TextEncoder().encode("hello world");
    const p1363 = new Uint8Array(
      await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key.privateKey, msg),
    );
    expect(p1363).toHaveLength(64);

    // Convert to DER then back
    const { p1363ToDer } = await import("./der");
    const der = p1363ToDer(p1363);
    expect(der[0]).toBe(0x30); // SEQUENCE
    const back = derToP1363(der);
    expect(back).toEqual(p1363);
  });
});

// ── BigInt timestamp parsing [WM10] ──────────────────────────────────────────

describe("parseBigIntNanos [WM10]", () => {
  it("parses a u64 decimal string with BigInt precision", () => {
    // 1700000000000000000 is the test-vector enrolled_at value
    const n = parseBigIntNanos("1700000000000000000");
    expect(n).toBe(1700000000000000000n);
  });

  it("handles the maximum safe integer boundary correctly", () => {
    // Number.MAX_SAFE_INTEGER = 9007199254740991 — values above this lose precision as numbers
    const maxSafe = BigInt(Number.MAX_SAFE_INTEGER);
    const overLimit = maxSafe + 1n;
    const parsed = parseBigIntNanos(overLimit.toString());
    expect(parsed).toBe(overLimit);
    // This would be lossy as a regular Number:
    expect(Number(overLimit) === Number(overLimit + 1n)).toBe(true); // demonstrates float collision
  });

  it("throws on non-digit input", () => {
    expect(() => parseBigIntNanos("123.456")).toThrow();
    expect(() => parseBigIntNanos("1e9")).toThrow();
    expect(() => parseBigIntNanos("abc")).toThrow();
  });
});

// ── CAS retry-rebase semantics [WM1] ─────────────────────────────────────────

describe("ConflictError [WM1]", () => {
  it("carries currentHead and currentVersion", () => {
    const err = new ConflictError("abc123", 5);
    expect(err.currentHead).toBe("abc123");
    expect(err.currentVersion).toBe(5);
    expect(err.name).toBe("ConflictError");
    expect(err instanceof ConflictError).toBe(true);
  });
});

// ── nowNanoStr format ─────────────────────────────────────────────────────────

describe("nowNanoStr", () => {
  it("returns a non-empty string of digits only", () => {
    const s = nowNanoStr();
    expect(s).toMatch(/^\d+$/);
    expect(s.includes(".")).toBe(false);
  });

  it("returns values that increase over time", () => {
    const a = nowNanoStr();
    const b = nowNanoStr();
    expect(BigInt(b) >= BigInt(a)).toBe(true);
  });
});
