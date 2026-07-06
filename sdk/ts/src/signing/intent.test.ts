// Golden-vector unit test for A4 intent signing: proves protobuf-es `toBinary(IntentBody)` stays
// byte-identical to Go `proto.Marshal` (the load-bearing assumption behind dropping ProtoWriter;
// see the T1 spike at sdk/ts/spike/wire-compat.test.ts) and that a TS-signed intent is a valid
// ECDSA-P256 signature over the domain-prefixed body.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { buildIntentBodyBytes, buildSignedIntent } from "./intent.js";
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

  const signed = await buildSignedIntent("create-spawn", bodyBytes, pair.privateKey, spki);
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
