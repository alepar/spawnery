/**
 * Session-key crypto primitives: sign, export, hash, self-check.
 *
 * The session key is an ECDSA P-256 non-extractable CryptoKey held by a KeyStore
 * (see ../keystore.ts). Key creation/persistence (getOrCreateSessionKey,
 * IDBKeyStore/MemoryKeyStore) stays app-side — the SDK only signs with a keypair
 * it is handed.
 */

import { sha256 } from "./encoding";

/**
 * exportSpkiDer returns the DER SPKI of the public key as Uint8Array.
 * Matches Go's x509.MarshalPKIXPublicKey and WebCrypto exportKey('spki').
 */
export async function exportSpkiDer(publicKey: CryptoKey): Promise<Uint8Array> {
  const buf = await crypto.subtle.exportKey("spki", publicKey);
  return new Uint8Array(buf as ArrayBuffer);
}

/**
 * sessionKeyHash returns SHA-256(DER SPKI) — the cnf claim value [AM11].
 * Matches token.SessionKeyHash on the Go side.
 */
export async function sessionKeyHash(spkiDer: Uint8Array): Promise<Uint8Array> {
  return sha256(spkiDer);
}

/**
 * keyCanSign performs a positive self-check: sign a fixed probe message and
 * verify it succeeds. Returns false if the key is gone or signing fails for any reason.
 * Called before every /refresh to detect key eviction (ITP, storage pressure).
 */
export async function keyCanSign(privateKey: CryptoKey): Promise<boolean> {
  try {
    const probe = new TextEncoder().encode("spawnery/session-key-probe/v1");
    await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, probe);
    return true;
  } catch {
    return false;
  }
}

/**
 * signP1363 signs msg with privateKey using ECDSA P-256 SHA-256 and returns the
 * P1363 raw 64-byte r||s signature (WebCrypto ECDSA native format).
 * Used for PoP and intent signing.
 */
export async function signP1363(privateKey: CryptoKey, msg: Uint8Array): Promise<Uint8Array> {
  const sig = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" },
    privateKey,
    msg as unknown as Uint8Array<ArrayBuffer>,
  );
  return new Uint8Array(sig as ArrayBuffer);
}
