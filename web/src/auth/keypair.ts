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
 *
 * signP1363/exportSpkiDer/sessionKeyHash/keyCanSign are sourced from @spawnery/client (pure
 * crypto, environment-neutral). Key creation/persistence (getOrCreateSessionKey, loadSessionKey,
 * clearSessionKey) stay app-side: they need navigator.storage + the browser/test KeyStore.
 */

export { signP1363, exportSpkiDer, sessionKeyHash, keyCanSign } from "@spawnery/client";
import type { KeyStore, SessionKeyPair } from "./keystore";

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
