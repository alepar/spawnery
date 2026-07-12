// Golden-vector unit test for A4 intent signing: proves protobuf-es `toBinary(IntentBody)` stays
// byte-identical to Go `proto.Marshal` (the load-bearing assumption behind dropping ProtoWriter;
// see the T1 spike at sdk/ts/spike/wire-compat.test.ts) and that a TS-signed intent is a valid
// ECDSA-P256 signature over the domain-prefixed body.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { buildIntentBodyBytes, buildSignedIntent, pollAndSign } from "./intent.js";
import type { SessionSigner } from "./sessionSigner.js";
import { WebCryptoSessionSigner } from "./sessionSigner.js";
import { sessionKeyHash } from "../keys/crypto.js";

const toHex = (u: Uint8Array): string => Buffer.from(u).toString("hex");
const fromHex = (s: string): Uint8Array => new Uint8Array(Buffer.from(s, "hex"));

// internal/intent/testdata/intent_vectors.json, resolved relative to this file (repo root is four
// levels up from sdk/ts/src/signing/).
const VECTORS_PATH = fileURLToPath(
  new URL("../../../../internal/intent/testdata/intent_vectors.json", import.meta.url),
);
const vectors = JSON.parse(readFileSync(VECTORS_PATH, "utf8")) as {
  spki_der_hex: string;
  spki_hash_hex: string;
  body_bytes_hex: string;
};

// GOLDEN_WITH_MOUNTS reuses the T1 spike's repeated-MountRef vector (Go-produced; see
// sdk/ts/spike/genvec/main.go), asserting the with-mounts path still byte-matches after dropping
// ProtoWriter for the no-mounts case above.
const GOLDEN_WITH_MOUNTS =
  "0a0a6d6f756e74732d76656310819d80cc061a0a73702d7665632d3030322007320c6372656174652d737061776e3a0e6170702f6d407368613235363a3162180a0773637261746368120b736372617463683a2f2f732001621c0a026768120867683a2f2f6f2f721a057365632d312a053132333435";

test("buildIntentBodyBytes matches the committed golden vector (no mounts)", () => {
  const bytes = buildIntentBodyBytes({
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
  assert.equal(toHex(bytes), vectors.body_bytes_hex);
});

test("pollAndSign verifies typed target metadata before signing and submits only the node token", async () => {
  const calls: string[] = [];
  const signer: SessionSigner = {
    publicSPKIDER: async () => new Uint8Array([1, 2, 3]),
    signP1363: async () => { calls.push("sign"); return new Uint8Array(64); },
  };
  let submitted: Record<string, unknown> | undefined;
  const pending = {
    op: "create-spawn", spawnId: "sp-1", generation: 7n, targetNodeId: "node-1",
    appRef: "app@sha256:1", image: "image@sha256:2", model: "m", dataRef: "",
    mounts: [{ name: "work", backendUri: "scratch://", credentialSecretId: "secret-1", createIfMissing: true, repositoryId: "repo-1" }],
    attachedSecretIds: ["secret-1"],
  };
  const client = {
    getPendingIntent: async () => ({
      ready: true, pending, nodeCertChain: new Uint8Array([9]), generation: 7n,
      targetNodeId: "node-1", targetNodeClass: "self-hosted", targetNodeAccountId: "acct-1",
    }),
    submitIntent: async (request: Record<string, unknown>) => { submitted = request; return {}; },
  };
  await pollAndSign({
    client: client as never,
    spawnId: "sp-1",
    pended: { ...pending },
    signer,
    nodeAccessToken: "node-token",
    verifyTarget: async (target) => {
      calls.push("verify");
      assert.equal(target.targetNodeId, "node-1");
      assert.equal(target.targetNodeClass, "self-hosted");
      assert.equal(target.targetNodeAccountId, "acct-1");
      assert.deepEqual(target.nodeCertChain, new Uint8Array([9]));
    },
    maxAttempts: 1,
  });
  assert.deepEqual(calls, ["verify", "sign"]);
  assert.equal(submitted?.nodeAccessToken, "node-token");
});

test("pollAndSign rejects target or locally-pended substitutions before signing or submitting", async (t) => {
  const basePending = {
    op: "create-spawn", spawnId: "sp-1", generation: 7n, targetNodeId: "node-1",
    appRef: "app@sha256:1", image: "image@sha256:2", model: "m", dataRef: "data-1",
    mounts: [{ name: "work", backendUri: "scratch://", credentialSecretId: "secret-1", createIfMissing: true, repositoryId: "repo-1" }],
    attachedSecretIds: ["secret-1"],
  };
  const mutations: Array<[string, (response: Record<string, unknown>, pending: Record<string, unknown>) => void]> = [
    ["response generation", (r) => { r.generation = 8n; }],
    ["response target", (r) => { r.targetNodeId = "node-2"; }],
    ["operation", (_r, p) => { p.op = "resume-spawn"; }],
    ["spawn", (_r, p) => { p.spawnId = "sp-2"; }],
    ["generation", (_r, p) => { p.generation = 8n; }],
    ["target", (_r, p) => { p.targetNodeId = "node-2"; }],
    ["app", (_r, p) => { p.appRef = "other"; }],
    ["image", (_r, p) => { p.image = "other"; }],
    ["model", (_r, p) => { p.model = "other"; }],
    ["data ref", (_r, p) => { p.dataRef = "other"; }],
    ["mount", (_r, p) => { p.mounts = [{ ...basePending.mounts[0], backendUri: "other" }]; }],
    ["attached secrets", (_r, p) => { p.attachedSecretIds = ["other"]; }],
  ];
  for (const [name, mutate] of mutations) {
    await t.test(name, async () => {
      let signed = false;
      let submitted = false;
      const response: Record<string, unknown> = {
        ready: true, pending: structuredClone(basePending), nodeCertChain: new Uint8Array([9]),
        generation: 7n, targetNodeId: "node-1", targetNodeClass: "self-hosted", targetNodeAccountId: "acct-1",
      };
      mutate(response, response.pending as Record<string, unknown>);
      await assert.rejects(pollAndSign({
        client: {
          getPendingIntent: async () => response,
          submitIntent: async () => { submitted = true; return {}; },
        } as never,
        spawnId: "sp-1",
        pended: structuredClone(basePending),
        signer: {
          publicSPKIDER: async () => new Uint8Array([1]),
          signP1363: async () => { signed = true; return new Uint8Array(64); },
        },
        nodeAccessToken: "node-token",
        verifyTarget: async () => {},
        maxAttempts: 1,
      }));
      assert.equal(signed, false);
      assert.equal(submitted, false);
    });
  }
});

test("buildIntentBodyBytes matches the golden vector with repeated MountRef", () => {
  const bytes = buildIntentBodyBytes({
    jti: "mounts-vec",
    issuedAt: 1770000001,
    spawnId: "sp-vec-002",
    generation: 7n,
    targetNodeId: "",
    op: "create-spawn",
    appRef: "app/m@sha256:1",
    image: "",
    model: "",
    dataRef: "",
    sessionId: "",
    mounts: [
      { name: "scratch", backendUri: "scratch://s", createIfMissing: true },
      { name: "gh", backendUri: "gh://o/r", credentialSecretId: "sec-1", repositoryId: "12345" },
    ],
  });
  assert.equal(toHex(bytes), GOLDEN_WITH_MOUNTS);
});

test("sessionKeyHash matches the committed cnf-hash golden vector", async () => {
  const spki = fromHex(vectors.spki_der_hex);
  const hash = await sessionKeyHash(spki);
  assert.equal(toHex(hash), vectors.spki_hash_hex);
});

test("buildSignedIntent produces a verifiable ECDSA-P256 signature (self-consistency)", async () => {
  // ECDSA signatures are randomized (k is per-signature), so this can't golden-match a fixed sig —
  // instead round-trip: sign, then verify with WebCrypto against the same domain||body message
  // buildSignedIntent signs.
  const pair = (await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    false, // non-extractable private key, matching production session keys
    ["sign", "verify"],
  )) as CryptoKeyPair;
  const spki = new Uint8Array(await crypto.subtle.exportKey("spki", pair.publicKey));

  const bodyBytes = buildIntentBodyBytes({
    jti: "self-check",
    issuedAt: 1770000002,
    spawnId: "sp-self-001",
    generation: 1n,
    targetNodeId: "",
    op: "create-spawn",
    appRef: "app/self@sha256:1",
    image: "",
    model: "",
    dataRef: "",
    sessionId: "",
    mounts: [],
  });

  const signed = await buildSignedIntent("create-spawn", bodyBytes, new WebCryptoSessionSigner(pair.privateKey, pair.publicKey));
  assert.equal(signed.domain, "spawnery/intent/create-spawn/v1");
  assert.deepEqual(signed.body, bodyBytes);

  const domainBytes = new TextEncoder().encode(signed.domain);
  const msg = new Uint8Array(domainBytes.length + bodyBytes.length);
  msg.set(domainBytes);
  msg.set(bodyBytes, domainBytes.length);

  const ok = await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    pair.publicKey,
    signed.sig as unknown as Uint8Array<ArrayBuffer>,
    msg as unknown as Uint8Array<ArrayBuffer>,
  );
  assert.equal(ok, true);
});
