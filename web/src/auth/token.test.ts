/**
 * Tests for session token wire parsing.
 *
 * Uses a fixture token built from known values to verify:
 * - Wire splitting (base64url body + sig)
 * - Proto3 field decoding
 * - BigInt expiresAt precision (WM10)
 */

import { describe, it, expect } from "vitest";
import { ProtoWriter } from "./protobuf";
import { parseTokenWire, decodeSessionTokenBody, parseAccessToken, fromBase64Url, toBase64Url } from "./token";

// Build a fixture token body (no real sig needed — SPA never verifies sig).
function buildTokenBody(opts: {
  accountId?: string;
  handle?: string;
  expiresAt?: bigint;
  sessionKeyHash?: Uint8Array;
}): Uint8Array {
  const w = new ProtoWriter();
  if (opts.accountId) w.writeBytes(1, opts.accountId);
  if (opts.handle) w.writeBytes(2, opts.handle);
  if (opts.expiresAt !== undefined) w.writeVarint(6, opts.expiresAt);
  if (opts.sessionKeyHash) w.writeBytes(7, opts.sessionKeyHash);
  return w.finish();
}

function makeWireToken(bodyBytes: Uint8Array): string {
  const fakeSig = new Uint8Array(64); // zero sig — SPA ignores sig
  return toBase64Url(bodyBytes) + "." + toBase64Url(fakeSig);
}

describe("parseTokenWire", () => {
  it("splits wire into body and sig parts", () => {
    const body = new Uint8Array([1, 2, 3]);
    const sig = new Uint8Array([4, 5, 6]);
    const wire = toBase64Url(body) + "." + toBase64Url(sig);
    const { bodyBytes, sigBytes } = parseTokenWire(wire);
    expect(Array.from(bodyBytes)).toEqual([1, 2, 3]);
    expect(Array.from(sigBytes)).toEqual([4, 5, 6]);
  });

  it("throws on missing dot", () => {
    expect(() => parseTokenWire("nodot")).toThrow("malformed");
  });
});

describe("decodeSessionTokenBody", () => {
  it("decodes all fields correctly", () => {
    const hash = new Uint8Array(32).fill(0xab);
    const body = buildTokenBody({
      accountId: "acc-123",
      handle: "alice",
      expiresAt: 9999999999n,
      sessionKeyHash: hash,
    });
    const dec = decodeSessionTokenBody(body);
    expect(dec.accountId).toBe("acc-123");
    expect(dec.handle).toBe("alice");
    expect(dec.expiresAt).toBe(9999999999n);
    expect(Array.from(dec.sessionKeyHash)).toEqual(Array.from(hash));
  });

  it("returns empty defaults for missing fields", () => {
    const dec = decodeSessionTokenBody(new Uint8Array(0));
    expect(dec.accountId).toBe("");
    expect(dec.handle).toBe("");
    expect(dec.expiresAt).toBe(0n);
    expect(dec.sessionKeyHash.length).toBe(0);
  });

  it("BigInt expiresAt handles large values without float precision loss (WM10)", () => {
    // 2^53 + 1 exceeds Number.MAX_SAFE_INTEGER — BigInt must be used.
    const large = BigInt(Number.MAX_SAFE_INTEGER) + 1n;
    const body = buildTokenBody({ expiresAt: large });
    const dec = decodeSessionTokenBody(body);
    expect(dec.expiresAt).toBe(large);
  });
});

describe("parseAccessToken round-trip", () => {
  it("parses a full wire token", () => {
    const hash = new Uint8Array(32).fill(0xcd);
    const body = buildTokenBody({
      accountId: "user-xyz",
      handle: "bob",
      expiresAt: 1800000000n,
      sessionKeyHash: hash,
    });
    const wire = makeWireToken(body);
    const dec = parseAccessToken(wire);
    expect(dec.accountId).toBe("user-xyz");
    expect(dec.handle).toBe("bob");
    expect(dec.expiresAt).toBe(1800000000n);
    expect(Array.from(dec.sessionKeyHash)).toEqual(Array.from(hash));
    // bodyBytes preserved verbatim
    expect(Array.from(dec.bodyBytes)).toEqual(Array.from(body));
  });
});

describe("base64url helpers", () => {
  it("round-trips bytes", () => {
    for (const len of [0, 1, 2, 3, 63, 64, 65]) {
      const orig = new Uint8Array(len).map((_, i) => i % 256);
      const encoded = toBase64Url(orig);
      expect(encoded).not.toContain("="); // no padding
      expect(encoded).not.toContain("+");
      expect(encoded).not.toContain("/");
      const decoded = fromBase64Url(encoded);
      expect(Array.from(decoded)).toEqual(Array.from(orig));
    }
  });
});
