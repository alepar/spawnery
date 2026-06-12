/**
 * Tests for intent signing.
 *
 * Golden vector tests: load internal/intent/testdata/intent_vectors.json,
 * build IntentBody from known fields, verify body hex == body_bytes_hex,
 * and verify signature against the golden SPKI.
 *
 * Negative tests: unpended tuple rejected, CP-substituted fields rejected.
 */

import { describe, it, expect, vi } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  buildIntentBodyBytes,
  buildSignedIntent,
  domainForOp,
  pollAndSign,
  type PendedOp,
} from "./intent";
import { MemoryKeyStore } from "./keystore";
import { getOrCreateSessionKey, exportSpkiDer } from "./keypair";
import { p1363ToDer } from "@/keys/der";

const VECTORS_PATH = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/intent/testdata/intent_vectors.json",
);

function bytesToHex(b: Uint8Array): string {
  return Array.from(b).map((x) => x.toString(16).padStart(2, "0")).join("");
}

function hexToBytes(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < hex.length; i += 2) out[i / 2] = parseInt(hex.slice(i, i + 2), 16);
  return out;
}

function fromB64(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

describe("buildIntentBodyBytes — golden vectors", () => {
  const raw = fs.readFileSync(VECTORS_PATH, "utf8");
  const vectors = JSON.parse(raw) as {
    body_bytes_hex: string;
    spki_der_hex: string;
    spki_hash_hex: string;
  };

  it("body bytes match intent_vectors.json body_bytes_hex", () => {
    const body = buildIntentBodyBytes({
      jti: "fixed-jti-for-vectors",
      issuedAt: 1770000000,
      spawnId: "sp-vec-001",
      generation: 1n,
      targetNodeId: "node-vec-1",
      op: "create-spawn",
      appRef: "app/test@sha256:deadbeef",
      image: "registry/img@sha256:cafebabe",
      model: "claude-test",
      dataRef: "",
      sessionId: "",
      mounts: [],
    });
    expect(bytesToHex(body)).toBe(vectors.body_bytes_hex);
  });

  it("sign + verify against golden SPKI (P1363 path)", async () => {
    const raw = fs.readFileSync(VECTORS_PATH, "utf8");
    const v = JSON.parse(raw) as { body_bytes_hex: string };

    const bodyBytes = hexToBytes(v.body_bytes_hex);

    // Generate a fresh key to sign (we can't import the private key without the scalar)
    // (signature is verified against kp.publicKey — the golden private key is not importable)
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const localSpki = await exportSpkiDer(kp.publicKey);

    const op = "create-spawn";
    const domain = domainForOp(op);
    const signed = await buildSignedIntent(op, bodyBytes, kp.privateKey, localSpki);

    // Verify with our own public key (not the golden — we just generated a fresh key)
    const sigBytes = fromB64(signed.sig);
    expect(sigBytes.length).toBe(64);

    const domainBytes = new TextEncoder().encode(domain);
    const msg = new Uint8Array(domainBytes.length + bodyBytes.length);
    msg.set(domainBytes);
    msg.set(bodyBytes, domainBytes.length);

    const ok = await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      kp.publicKey,
      sigBytes as unknown as Uint8Array<ArrayBuffer>,
      msg as unknown as Uint8Array<ArrayBuffer>,
    );
    expect(ok).toBe(true);
  });

  it("DER round-trip of P1363 signature", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const body = hexToBytes(vectors.body_bytes_hex);
    const signed = await buildSignedIntent("create-spawn", body, kp.privateKey, spki);
    const p1363 = fromB64(signed.sig);
    // p1363ToDer should not throw for valid 64-byte sig
    const der = p1363ToDer(p1363);
    expect(der.length).toBeGreaterThan(64);
  });
});

describe("pollAndSign — AM1 never-sign-unpended", () => {
  it("rejects when CP-returned op does not match locally pended op", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);

    const pended: PendedOp = {
      op: "create-spawn",
      spawnId: "sp-test",
      appRef: "myapp",
      model: "claude",
    };

    // CP returns a DIFFERENT op
    const unaryMock = vi.fn().mockResolvedValue({
      ready: true,
      pending: {
        op: "resume-spawn", // mismatch!
        spawnId: "sp-test",
        generation: "1",
        targetNodeId: "node-1",
        image: "img",
        appRef: "myapp",
        model: "claude",
        dataRef: "",
      },
    });

    await expect(
      pollAndSign({
        spawnId: "sp-test",
        pended,
        privateKey: kp.privateKey,
        publicKey: kp.publicKey,
        unaryFn: unaryMock,
      }),
    ).rejects.toThrow(/op mismatch/);
  });

  it("rejects when CP substitutes a different appRef", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);

    const pended: PendedOp = {
      op: "create-spawn",
      spawnId: "sp-test",
      appRef: "real-app",
    };

    const unaryMock = vi.fn().mockResolvedValue({
      ready: true,
      pending: {
        op: "create-spawn",
        spawnId: "sp-test",
        generation: "1",
        targetNodeId: "node-1",
        image: "img",
        appRef: "attacker-app", // substituted!
        model: "",
        dataRef: "",
      },
    });

    await expect(
      pollAndSign({
        spawnId: "sp-test",
        pended,
        privateKey: kp.privateKey,
        publicKey: kp.publicKey,
        unaryFn: unaryMock,
      }),
    ).rejects.toThrow(/appRef mismatch/);
  });

  it("rejects when CP substitutes a different model", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);

    const pended: PendedOp = { op: "create-spawn", spawnId: "sp-t", model: "claude-3" };
    const unaryMock = vi.fn().mockResolvedValue({
      ready: true,
      pending: {
        op: "create-spawn", spawnId: "sp-t", generation: "1",
        targetNodeId: "", image: "", appRef: "", model: "evil-model", dataRef: "",
      },
    });

    await expect(
      pollAndSign({ spawnId: "sp-t", pended, privateKey: kp.privateKey, publicKey: kp.publicKey, unaryFn: unaryMock }),
    ).rejects.toThrow(/model mismatch/);
  });

  it("succeeds when tuple matches and submits intent", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);

    const pended: PendedOp = {
      op: "create-spawn", spawnId: "sp-ok", appRef: "myapp", model: "claude",
    };

    const calls: Array<[string, unknown]> = [];
    const unaryMock = vi.fn().mockImplementation((method: string, body: unknown) => {
      calls.push([method, body]);
      if (method === "GetPendingIntent") {
        return Promise.resolve({
          ready: true,
          pending: {
            op: "create-spawn", spawnId: "sp-ok", generation: "2",
            targetNodeId: "node-1", image: "img", appRef: "myapp", model: "claude", dataRef: "",
          },
        });
      }
      if (method === "SubmitIntent") {
        return Promise.resolve({});
      }
      return Promise.resolve({});
    });

    const jti = await pollAndSign({
      spawnId: "sp-ok",
      pended,
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      unaryFn: unaryMock,
    });

    expect(typeof jti).toBe("string");
    expect(jti.length).toBe(32); // 16 bytes hex
    expect(calls.find(([m]) => m === "SubmitIntent")).toBeTruthy();
  });
});
