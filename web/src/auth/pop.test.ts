/**
 * Tests for PoP construction.
 *
 * Golden vector: known key + hash + ts + nonce → verify signature with WebCrypto.
 * Also verifies header encoding (base64url unpadded, decimal timestamp).
 */

import { describe, it, expect } from "vitest";
import { buildPoP } from "./pop";
import { fromBase64Url } from "./token";
import { MemoryKeyStore } from "./keystore";
import { getOrCreateSessionKey } from "./keypair";
import { mapNackCode, mapAsError, isCnfMismatch } from "./errors";

const POP_DOMAIN = "spawnery/refresh-pop/v1";

async function makeKey() {
  const store = new MemoryKeyStore();
  return getOrCreateSessionKey(store);
}

describe("buildPoP", () => {
  it("produces a verifiable P1363 signature over the correct message", async () => {
    const kp = await makeKey();
    const refreshTokenHash = new Uint8Array(32).fill(0xaa);
    const now = new Date(1700000000 * 1000); // fixed ts
    const nonce = new Uint8Array(16).fill(0xbb);

    const headers = await buildPoP(kp.privateKey, refreshTokenHash, now, nonce);

    // Verify timestamp
    expect(headers["X-PoP-Timestamp"]).toBe("1700000000");

    // Verify nonce encoding (base64url, no padding)
    const nonceDecoded = fromBase64Url(headers["X-PoP-Nonce"]);
    expect(Array.from(nonceDecoded)).toEqual(Array.from(nonce));
    expect(headers["X-PoP-Nonce"]).not.toContain("=");

    // Reconstruct the signed message
    const domainBytes = new TextEncoder().encode(POP_DOMAIN);
    const ts = 1700000000;
    const tsBytes = new Uint8Array(8);
    new DataView(tsBytes.buffer).setBigUint64(0, BigInt(ts), false);
    const msg = new Uint8Array(domainBytes.length + 32 + 8 + 16);
    let off = 0;
    msg.set(domainBytes, off); off += domainBytes.length;
    msg.set(refreshTokenHash, off); off += 32;
    msg.set(tsBytes, off); off += 8;
    msg.set(nonce, off);

    // Verify with WebCrypto
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

  it("uses random nonce when none injected", async () => {
    const kp = await makeKey();
    const hash = new Uint8Array(32);
    const h1 = await buildPoP(kp.privateKey, hash);
    const h2 = await buildPoP(kp.privateKey, hash);
    // Extremely unlikely to collide
    expect(h1["X-PoP-Nonce"]).not.toBe(h2["X-PoP-Nonce"]);
  });

  it("signature is base64url without padding", async () => {
    const kp = await makeKey();
    const headers = await buildPoP(kp.privateKey, new Uint8Array(32));
    expect(headers["X-PoP-Sig"]).not.toContain("=");
    expect(headers["X-PoP-Sig"]).not.toContain("+");
    expect(headers["X-PoP-Sig"]).not.toContain("/");
  });
});

describe("error mapping", () => {
  it("mapNackCode extracts known codes", () => {
    expect(mapNackCode("node rejected: CNF_MISMATCH for spawn")).toBe("CNF_MISMATCH");
    expect(mapNackCode("CORRESPONDENCE check failed")).toBe("CORRESPONDENCE");
    expect(mapNackCode("intent is STALE")).toBe("STALE");
    expect(mapNackCode("unknown error")).toBe("UNKNOWN_NACK");
  });

  it("mapAsError maps known codes", () => {
    expect(mapAsError("registration_closed")).toBe("registration_closed");
    expect(mapAsError("access_denied")).toBe("access_denied");
    expect(mapAsError("server_error")).toBe("server_error");
    expect(mapAsError("other")).toBe("unknown");
    expect(mapAsError(null)).toBe("unknown");
  });

  it("isCnfMismatch", () => {
    expect(isCnfMismatch("CNF_MISMATCH")).toBe(true);
    expect(isCnfMismatch("STALE")).toBe(false);
  });
});
