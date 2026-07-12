import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseCertDER, parseSPIFFEPrincipal, pemToDerList } from "./x509";

const VECTORS_FILE = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/secrets/subkey/testdata/subkey/verify_node.json",
);

interface Vector {
  leaf_pem: string;
}

function leafDER(): Uint8Array {
  const vector = JSON.parse(fs.readFileSync(VECTORS_FILE, "utf8")) as Vector;
  return Uint8Array.from(pemToDerList(vector.leaf_pem)[0]);
}

function findBytes(haystack: Uint8Array, needle: readonly number[]): number {
  outer: for (let i = 0; i <= haystack.length - needle.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer;
    }
    return i;
  }
  throw new Error(`test fixture does not contain ${needle.map((b) => b.toString(16)).join(" ")}`);
}

function firstUTCTime(der: Uint8Array): number {
  for (let i = 0; i < der.length - 15; i++) {
    if (der[i] === 0x17 && der[i + 1] === 13 && der[i + 14] === 0x5a) return i;
  }
  throw new Error("test fixture has no UTCTime");
}

describe("parseSPIFFEPrincipal canonical encoding", () => {
  const trustDomain = "prod.spawnery.internal";
  const valid = `spiffe://${trustDomain}/node/self-hosted/alice/node1`;

  it("accepts the exact canonical bytes", () => {
    expect(parseSPIFFEPrincipal(valid, trustDomain)).toMatchObject({
      kind: "node",
      role: "self-hosted",
      accountId: "alice",
      nodeId: "node1",
    });
  });

  it.each([
    `SPIFFE://${trustDomain}/node/self-hosted/alice/node1`,
    "spiffe://PROD.spawnery.internal/node/self-hosted/alice/node1",
    `spiffe://${trustDomain}:443/node/self-hosted/alice/node1`,
    `spiffe://alice@${trustDomain}/node/self-hosted/alice/node1`,
    `${valid}?`,
    `${valid}#fragment`,
    `spiffe://${trustDomain}/node/self-hosted/alice/%6eode1`,
    `spiffe://${trustDomain}/node/self-hosted/alice%2fnode1`,
    `spiffe://${trustDomain}/node//alice/node1`,
    `spiffe://${trustDomain}/node/../alice/node1`,
    `${valid}/`,
  ])("rejects non-canonical URI %s", (raw) => {
    expect(() => parseSPIFFEPrincipal(raw, trustDomain)).toThrow();
  });

  it("rejects a non-canonical configured trust domain", () => {
    expect(() => parseSPIFFEPrincipal(
      "spiffe://PROD.spawnery.internal/node/self-hosted/alice/node1",
      "PROD.spawnery.internal",
    )).toThrow("trust domain is not canonical");
  });
});

describe("parseCertDER strict DER", () => {
  it("rejects a non-digit RFC5280 time", () => {
    const der = leafDER();
    const time = firstUTCTime(der);
    der[time + 2] = 0x58;
    expect(() => parseCertDER(der)).toThrow("malformed RFC5280 time");
  });

  it("rejects an invalid calendar date instead of normalizing it", () => {
    const der = leafDER();
    const time = firstUTCTime(der);
    der.set([0x30, 0x32, 0x33, 0x31], time + 4); // February 31
    expect(() => parseCertDER(der)).toThrow("invalid RFC5280 calendar time");
  });

  it("applies the RFC5280 UTCTime 1950/2050 pivot", () => {
    const beforePivot = leafDER();
    const beforePivotTime = firstUTCTime(beforePivot);
    beforePivot.set([0x34, 0x39], beforePivotTime + 2);
    expect(parseCertDER(beforePivot).notBefore.getUTCFullYear()).toBe(2049);

    const atPivot = leafDER();
    const atPivotTime = firstUTCTime(atPivot);
    atPivot.set([0x35, 0x30], atPivotTime + 2);
    expect(parseCertDER(atPivot).notBefore.getUTCFullYear()).toBe(1950);
  });

  it("requires GeneralizedTime to have its exact RFC5280 length", () => {
    const der = leafDER();
    der[firstUTCTime(der)] = 0x18;
    expect(() => parseCertDER(der)).toThrow("malformed RFC5280 time");
  });

  it("rejects an Extension without an OID", () => {
    const der = leafDER();
    const oid = findBytes(der, [0x06, 0x03, 0x55, 0x1d, 0x11]);
    der[oid] = 0x05;
    expect(() => parseCertDER(der)).toThrow("Extension lacks OID");
  });

  it("rejects a non-DER critical BOOLEAN", () => {
    const der = leafDER();
    const basicConstraints = findBytes(der, [0x06, 0x03, 0x55, 0x1d, 0x13]);
    expect(Array.from(der.subarray(basicConstraints + 5, basicConstraints + 8))).toEqual([0x01, 0x01, 0xff]);
    der[basicConstraints + 7] = 0x01;
    expect(() => parseCertDER(der)).toThrow("critical BOOLEAN");
  });

  it("requires Extension.extnValue to be an OCTET STRING", () => {
    const der = leafDER();
    const san = findBytes(der, [0x06, 0x03, 0x55, 0x1d, 0x11]);
    const valueTag = der[san + 5] === 0x01 ? san + 8 : san + 5;
    expect(der[valueTag]).toBe(0x04);
    der[valueTag] = 0x02;
    expect(() => parseCertDER(der)).toThrow("not an OCTET STRING");
  });

  it("rejects an unrecognized critical extension", () => {
    const der = leafDER();
    const basicConstraints = findBytes(der, [0x06, 0x03, 0x55, 0x1d, 0x13]);
    der[basicConstraints + 4] = 0x7e;
    expect(() => parseCertDER(der)).toThrow("unrecognized critical extension");
  });

  it("rejects unsupported GeneralName choices", () => {
    const der = leafDER();
    const uri = new TextEncoder().encode("spiffe://prod.spawnery.internal/node/self-hosted/alice/node1");
    const uriValue = findBytes(der, Array.from(uri));
    expect(der[uriValue - 2]).toBe(0x86);
    der[uriValue - 2] = 0x81;
    expect(() => parseCertDER(der)).toThrow("unsupported GeneralName");
  });
});
