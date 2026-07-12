import { createHash } from "node:crypto";
import { describe, it, expect } from "vitest";
import { create, toBinary } from "@bufbuild/protobuf";
import { authv1 } from "@spawnery/client";
import { parseTokenWire, decodeSessionTokenBody, parseAccessToken, fromBase64Url, toBase64Url } from "./token";

function buildTokenBody(opts: {
  accountId?: string;
  handle?: string;
  expiresAt?: bigint;
  sessionKeyHash?: Uint8Array;
}): Uint8Array {
  return toBinary(authv1.SessionTokenBodySchema, create(authv1.SessionTokenBodySchema, {
    accountId: opts.accountId ?? "",
    handle: opts.handle ?? "",
    expiresAt: opts.expiresAt ?? 0n,
    sessionKeyHash: opts.sessionKeyHash ?? new Uint8Array(0),
  }));
}

function makeWireToken(bodyBytes: Uint8Array, artifactType = "session-token"): string {
  const artifact = create(authv1.SignedAuthArtifactSchema, {
    artifactType,
    payload: bodyBytes,
    signature: new Uint8Array(64).fill(0x5a),
    signerChain: [new Uint8Array([1, 2, 3]), new Uint8Array([4, 5, 6])],
    keyId: new Uint8Array(32).fill(0xa5),
  });
  return toBase64Url(toBinary(authv1.SignedAuthArtifactSchema, artifact));
}

describe("parseTokenWire", () => {
  it("decodes signed artifact metadata and preserves exact payload bytes", () => {
    const payload = new Uint8Array([1, 2, 3]);
    const parsed = parseTokenWire(makeWireToken(payload));
    expect(parsed.artifactType).toBe("session-token");
    expect(Array.from(parsed.payloadBytes)).toEqual([1, 2, 3]);
    expect(parsed.signatureBytes).toHaveLength(64);
    expect(parsed.signerChain.map((cert) => Array.from(cert))).toEqual([[1, 2, 3], [4, 5, 6]]);
    expect(parsed.keyId).toHaveLength(32);
  });

  it("rejects legacy and structurally invalid envelopes", () => {
    expect(() => parseTokenWire("YQ.Yg")).toThrow("malformed");
    expect(() => parseTokenWire(toBase64Url(new Uint8Array([0xff])))).toThrow();
    expect(() => parseTokenWire(makeWireToken(new Uint8Array([1]), "revocation-entry"))).toThrow("artifact type");

    const missing = create(authv1.SignedAuthArtifactSchema, { artifactType: "session-token" });
    expect(() => parseTokenWire(toBase64Url(toBinary(authv1.SignedAuthArtifactSchema, missing)))).toThrow("malformed");
  });
});

describe("decodeSessionTokenBody", () => {
  it("decodes all fields and preserves int64 precision", () => {
    const hash = new Uint8Array(32).fill(0xab);
    const large = BigInt(Number.MAX_SAFE_INTEGER) + 1n;
    const dec = decodeSessionTokenBody(buildTokenBody({ accountId: "acc-123", handle: "alice", expiresAt: large, sessionKeyHash: hash }));
    expect(dec.accountId).toBe("acc-123");
    expect(dec.handle).toBe("alice");
    expect(dec.expiresAt).toBe(large);
    expect(Array.from(dec.sessionKeyHash)).toEqual(Array.from(hash));
  });
});

describe("parseAccessToken", () => {
  it("decodes the session body from the exact envelope payload", () => {
    const body = buildTokenBody({ accountId: "user-xyz", handle: "bob", expiresAt: 1800000000n });
    const dec = parseAccessToken(makeWireToken(body));
    expect(dec.accountId).toBe("user-xyz");
    expect(dec.handle).toBe("bob");
    expect(dec.expiresAt).toBe(1800000000n);
    expect(Array.from(dec.bodyBytes)).toEqual(Array.from(body));
  });
});

const goVectorWire = "Cg1zZXNzaW9uLXRva2VuEkgKBmFjY3QtMRIFYWxpY2UaBXRvay0xIgJjcCiA4s-qBjCE6c-qBjogOBp1BBDlDyEJCW8_HUeGeL2p98Yo_b70ULu6GR0jfeUaQAxrJCOn5ps9b8sINCEcvl64ljxbrADgVUqOxo8w3WjDL3o2uMuAPsVCQf_u6NPsl5Al4IAOVO3xqpqxRUNzRQAiwQMwggG9MIIBY6ADAgECAhB2ZWN0b3ItbGVhZi0wMDAxMAoGCCqGSM49BAMCMCwxKjAoBgNVBAMTIVNwYXduZXJ5IHZlY3RvciBhdXRoIGludGVybWVkaWF0ZTAeFw0yMzExMTQyMTEzMjBaFw0yNDAyMTIyMjEzMjBaMCoxKDAmBgNVBAMTH1NwYXduZXJ5IHZlY3RvciBhcnRpZmFjdCBzaWduZXIwKjAFBgMrZXADIQAjvFSRLB5uksSoaCXIZ-J__cVVv_vUJE8Xomq__-6WXaOBlzCBlDAOBgNVHQ8BAf8EBAMCB4AwHwYDVR0jBBgwFoAUqfMA61lg6JEzr3NiARoeJvDi6i4wSAYDVR0RBEEwP4Y9c3BpZmZlOi8vcHJvZC5zcGF3bmVyeS5pbnRlcm5hbC9zaWduZXIvYXV0aC1hcnRpZmFjdC92ZWN0b3ItMTAXBgNVHSAEEDAOMAwGCisGAQQBg78wAQIwCgYIKoZIzj0EAwIDSAAwRQIgO-FC6zHKCvMYrNBa5JWpzGUJEyc2gC8bDsA8jJ_F07ICIQCdw5wTrHs21vX4KlfppXXJ6nF4QN-lrgw2_SsletWuNCLKAzCCAcYwggFroAMCAQICEHZlY3Rvci1pbnRlci0wMDEwCgYIKoZIzj0EAwIwHzEdMBsGA1UEAxMUU3Bhd25lcnkgdmVjdG9yIHJvb3QwHhcNMjMxMTE0MTAxMzIwWhcNMjQwNTEyMjIxMzIwWjAsMSowKAYDVQQDEyFTcGF3bmVyeSB2ZWN0b3IgYXV0aCBpbnRlcm1lZGlhdGUwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAR88nsYjQNPfopSOAMEtRrDwIlp4nfyGzWmC0j8R2aZeAd3VRDbjtBAKT2axp90MNu6fa3mPOmCKZ4Et50ieHPRo3wwejAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH_BAUwAwEB_zAdBgNVHQ4EFgQUqfMA61lg6JEzr3NiARoeJvDi6i4wHwYDVR0jBBgwFoAUaYvqY9xEo0RmP_FCmuoQhC3ye2swFwYDVR0gBBAwDjAMBgorBgEEAYO_MAEBMAoGCCqGSM49BAMCA0kAMEYCIQCI7Z9FaSO6JHUgS5fmaM-0tjLJERAri1BhMwEciWCGugIhANqvCJR2CK4nE8tIp1_4d0flrdDScI-fZ-VSi_VzJeLiKiDfzL_78Pi4HSXjxSI-b3QG-K0iI58kBBNmqdjytejjzg";

describe("Go envelope vector", () => {
  it("observes identical payload, key ID, and certificate hashes", () => {
    const parsed = parseTokenWire(goVectorWire);
    const hex = (bytes: Uint8Array) => Buffer.from(bytes).toString("hex");
    const hash = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");
    expect(hex(parsed.payloadBytes)).toBe("0a06616363742d311205616c6963651a05746f6b2d31220263702880e2cfaa063084e9cfaa063a20381a750410e50f2109096f3f1d478678bda9f7c628fdbef450bbba191d237de5");
    expect(hex(parsed.keyId)).toBe("dfccbffbf0f8b81d25e3c5223e6f7406f8ad22239f24041366a9d8f2b5e8e3ce");
    expect(parsed.signerChain.map(hash)).toEqual([
      "76caadf31f71eb8e0f6bd2fc4707bf59c9cff3a8361cab3dc0d4852c074ee226",
      "74a5ecad98438a8302afe66781fa05ac8d4287106e1f9df69c42a680e84c7ad6",
    ]);
  });
});

describe("base64url helpers", () => {
  it("round-trips bytes without padding", () => {
    for (const len of [0, 1, 2, 3, 63, 64, 65]) {
      const original = new Uint8Array(len).map((_, i) => i % 256);
      const encoded = toBase64Url(original);
      expect(encoded).not.toMatch(/[=+/]/);
      expect(Array.from(fromBase64Url(encoded))).toEqual(Array.from(original));
    }
  });
});
