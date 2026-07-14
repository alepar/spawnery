import { test } from "node:test";
import assert from "node:assert/strict";
import { WebCryptoSessionSigner } from "./sessionSigner.js";

test("WebCryptoSessionSigner uses the supplied non-extractable key and signs exact domain plus body", async () => {
  const pair = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["sign", "verify"],
  ) as CryptoKeyPair;
  assert.equal(pair.privateKey.extractable, false);

  const signer = new WebCryptoSessionSigner(pair.privateKey, pair.publicKey);
  const spki = await signer.publicSPKIDER();
  const body = new Uint8Array([0, 1, 2, 255]);
  const domain = "spawnery/intent/test/v1";
  const signature = await signer.signP1363(domain, body);

  assert.equal(signature.length, 64);
  const domainBytes = new TextEncoder().encode(domain);
  const exact = new Uint8Array(domainBytes.length + body.length);
  exact.set(domainBytes);
  exact.set(body, domainBytes.length);
  assert.equal(await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    pair.publicKey,
    signature,
    exact,
  ), true);
  assert.deepEqual(spki, new Uint8Array(await crypto.subtle.exportKey("spki", pair.publicKey)));
});

test("WebCryptoSessionSigner does not generate a replacement key", async () => {
  const pair = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"],
  ) as CryptoKeyPair;
  const originalGenerateKey = crypto.subtle.generateKey.bind(crypto.subtle);
  let generated = false;
  Object.defineProperty(crypto.subtle, "generateKey", {
    configurable: true,
    value: async () => { generated = true; throw new Error("must not generate"); },
  });
  try {
    const signer = new WebCryptoSessionSigner(pair.privateKey, pair.publicKey);
    await signer.signP1363("domain", new Uint8Array([1]));
    assert.equal(generated, false);
  } finally {
    Object.defineProperty(crypto.subtle, "generateKey", { configurable: true, value: originalGenerateKey });
  }
});
