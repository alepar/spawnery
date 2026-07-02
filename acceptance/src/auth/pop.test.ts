/**
 * Golden vector: known key + hash + ts + nonce -> verify signature with WebCrypto. Mirrors
 * web/src/auth/pop.test.ts's structure (same wire contract, independent Node port).
 */

import { describe, it, expect } from "vitest";
import { buildPoP, fromBase64Url, toBase64Url, sha256 } from "./pop";

const POP_DOMAIN = "spawnery/refresh-pop/v1";

async function makeKey(): Promise<CryptoKeyPair> {
  return (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
}

describe("buildPoP", () => {
  it("produces a verifiable P1363 signature over the correct message", async () => {
    const kp = await makeKey();
    const refreshTokenHash = new Uint8Array(32).fill(0xaa);
    const now = new Date(1700000000 * 1000); // fixed ts
    const nonce = new Uint8Array(16).fill(0xbb);

    const headers = await buildPoP(kp.privateKey, refreshTokenHash, now, nonce);

    expect(headers["X-PoP-Timestamp"]).toBe("1700000000");

    const nonceDecoded = fromBase64Url(headers["X-PoP-Nonce"]);
    expect(Array.from(nonceDecoded)).toEqual(Array.from(nonce));
    expect(headers["X-PoP-Nonce"]).not.toContain("=");

    // Reconstruct the signed message per the frozen wire contract (domain || hash || be64(ts) || nonce).
    const domainBytes = new TextEncoder().encode(POP_DOMAIN);
    const tsBytes = new Uint8Array(8);
    new DataView(tsBytes.buffer).setBigUint64(0, 1700000000n, false);
    const msg = new Uint8Array(domainBytes.length + 32 + 8 + 16);
    let off = 0;
    msg.set(domainBytes, off); off += domainBytes.length;
    msg.set(refreshTokenHash, off); off += 32;
    msg.set(tsBytes, off); off += 8;
    msg.set(nonce, off);

    const sig = fromBase64Url(headers["X-PoP-Sig"]);
    expect(sig.length).toBe(64);
    expect(headers["X-PoP-Sig"]).not.toContain("=");
    expect(headers["X-PoP-Sig"]).not.toContain("+");
    expect(headers["X-PoP-Sig"]).not.toContain("/");

    const ok = await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      kp.publicKey,
      sig as unknown as Uint8Array<ArrayBuffer>,
      msg as unknown as Uint8Array<ArrayBuffer>,
    );
    expect(ok).toBe(true);
  });

  it("uses a random nonce when none is injected", async () => {
    const kp = await makeKey();
    const hash = new Uint8Array(32);
    const h1 = await buildPoP(kp.privateKey, hash);
    const h2 = await buildPoP(kp.privateKey, hash);
    expect(h1["X-PoP-Nonce"]).not.toBe(h2["X-PoP-Nonce"]);
  });

  it("signature and nonce are base64url without padding", async () => {
    const kp = await makeKey();
    const headers = await buildPoP(kp.privateKey, new Uint8Array(32));
    for (const v of [headers["X-PoP-Sig"], headers["X-PoP-Nonce"]]) {
      expect(v).not.toContain("=");
      expect(v).not.toContain("+");
      expect(v).not.toContain("/");
    }
  });
});

describe("toBase64Url / fromBase64Url", () => {
  it("round-trips arbitrary bytes", () => {
    const bytes = new Uint8Array([0, 1, 2, 253, 254, 255, 16, 32]);
    expect(fromBase64Url(toBase64Url(bytes))).toEqual(bytes);
  });

  it("never emits padding or +/ characters", () => {
    // 32 0xff bytes is chosen to stress every base64 sextet boundary.
    const bytes = new Uint8Array(32).fill(0xff);
    const s = toBase64Url(bytes);
    expect(s).not.toMatch(/[+/=]/);
  });
});

describe("sha256", () => {
  it("matches the known SHA-256(\"abc\") test vector", async () => {
    const digest = await sha256(new TextEncoder().encode("abc"));
    expect(Buffer.from(digest).toString("hex")).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
  });
});
