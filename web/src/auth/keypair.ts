/**
 * Session keypair lifecycle: generate, persist, export, hash, sign, self-check.
 *
 * The session key is an ECDSA P-256 non-extractable CryptoKey stored in the
 * KeyStore. It is distinct from the device key in keys/device.ts.
 *
 * Security properties:
 * - Non-extractable: XSS can use but not exfiltrate the private key.
 * - navigator.storage.persist() called at creation (ROAST: persist-at-create).
 * - keyCanSign self-check before every /refresh call (positive proof).
 */

import { sha256 } from "@/keys/encoding";
import { KeyStore, SessionKeyPair } from "./keystore";

// StorageNavigator lets tests inject a mock navigator.storage.
export interface StorageNavigator {
  persist?(): Promise<boolean>;
}

/**
 * getOrCreateSessionKey loads the session keypair from the store; creates and
 * persists a new one if absent. Calls navigator.storage.persist() on creation.
 *
 * @param store - Injectable KeyStore (IDBKeyStore in prod, MemoryKeyStore in tests).
 * @param storageNav - Injectable navigator.storage (defaults to real navigator.storage).
 */
export async function getOrCreateSessionKey(
  store: KeyStore,
  storageNav: StorageNavigator = navigator.storage ?? {},
): Promise<SessionKeyPair> {
  const existing = await store.get();
  if (existing) return existing;

  // Generate fresh non-extractable ECDSA P-256 keypair.
  // Include "verify" so the public key gets ["verify"] usage — needed for cross-language
  // signature verification and self-check in tests.
  const pair = (await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    false, // non-extractable private key
    ["sign", "verify"],
  )) as CryptoKeyPair;

  const kp: SessionKeyPair = { privateKey: pair.privateKey, publicKey: pair.publicKey };
  await store.put(kp);

  // Request persistent storage at creation time so the browser doesn't evict
  // the IndexedDB under storage pressure (ROAST: persist-at-create).
  if (storageNav.persist) {
    await storageNav.persist().catch(() => {
      // Best-effort; failure is logged externally if needed.
    });
  }

  return kp;
}

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

/**
 * loadSessionKey loads the session keypair from the store. Returns null if absent.
 *
 * Use this on restore/refresh paths — callers treat null as key-lost and route to
 * recovery rather than silently minting a fresh keypair. Reserve getOrCreateSessionKey
 * for the explicit login action in LoginView.
 */
export async function loadSessionKey(store: KeyStore): Promise<SessionKeyPair | null> {
  return store.get();
}

/** clearSessionKey removes the session keypair from the store (key loss / logout). */
export async function clearSessionKey(store: KeyStore): Promise<void> {
  await store.delete();
}
