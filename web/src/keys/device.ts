/**
 * Web device key management (Phase 3, [M15][WM9][WM11]).
 *
 * Generates and persists non-extractable WebCrypto device keypairs:
 *   - X25519 for HPKE sealing/unsealing (extractable:false in IndexedDB)
 *   - ECDSA-P256 for device-set entry signing (extractable:false in IndexedDB)
 *
 * Keys are stored in IndexedDB under the "device-keys" object store. The
 * extractable:false invariant means XSS can use a live key but cannot exfiltrate
 * it (spec §1, [roast M15]).
 *
 * Feature detection: X25519 must be supported natively — no extractable
 * polyfill. Browsers without native X25519 get an "unsupported" error.
 */

import { hkdfExpand } from "./hkdf";
import { dhkemDerivePrivateScalar } from "./hpke";
import { mnemonicToSeed } from "./bip39";

const IDB_DB_NAME = "spawnery-device-keys";
const IDB_STORE = "keys";
const IDB_X25519_KEY = "x25519";
const IDB_ECDSA_KEY = "ecdsa-p256";
const IDB_X25519_PUB_KEY = "x25519-pub";
const IDB_ECDSA_PUB_KEY = "ecdsa-p256-pub";
const IDB_VERSION = 1;

// PKCS#8 DER prefix for P-256 private key (scalar is appended as 32 bytes).
// This encoding allows importing a raw P-256 scalar via WebCrypto.
// Structure: SEQUENCE { INTEGER(0), AlgorithmIdentifier(id-ecPublicKey, secp256r1), ECPrivateKey }
const PKCS8_P256_PREFIX = new Uint8Array([
  0x30, 0x41, 0x02, 0x01, 0x00, 0x30, 0x13, 0x06, 0x07,
  0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08,
  0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x04,
  0x27, 0x30, 0x25, 0x02, 0x01, 0x01, 0x04, 0x20,
]);

/** DeviceKeys holds the active non-extractable CryptoKey pair for a device. */
export interface DeviceKeys {
  /** Non-extractable X25519 private key for HPKE recipient-side deriveBits. */
  x25519Private: CryptoKey;
  /** X25519 public key (extractable) — registered in the device set. */
  x25519Public: CryptoKey;
  /** Non-extractable ECDSA-P256 private key for device-set entry signing. */
  ecdsaPrivate: CryptoKey;
  /** ECDSA-P256 public key (extractable, SEC1 uncompressed) — registered in the device set. */
  ecdsaPublic: CryptoKey;
}

/** DeviceRef is the public identity registered in the device-set log. */
export interface DeviceRef {
  /** Raw 32-byte X25519 public key (Uint8Array). */
  x25519Pub: Uint8Array;
  /** SEC1 uncompressed ECDSA-P256 public key (65 bytes: 0x04 || X || Y). */
  signPub: Uint8Array;
}

/**
 * featureDetectX25519 checks whether X25519 is supported natively.
 * Returns null on success or an error message describing the gap.
 * Non-extractable key gen cannot fall back to a polyfill.
 */
export async function featureDetectX25519(): Promise<string | null> {
  try {
    // generateKey always returns CryptoKeyPair for asymmetric algorithms; TS needs the cast.
    const k = await crypto.subtle.generateKey(
      { name: "X25519" },
      false,
      ["deriveBits"],
    ) as CryptoKeyPair;
    if (!k.privateKey) return "X25519 key generation returned no private key";
    return null;
  } catch {
    return "This browser does not support X25519 WebCrypto. Use a supported browser or spawnctl.";
  }
}

/**
 * generateDeviceKeys creates fresh non-extractable device keypairs.
 * Throws with the featureDetect message if X25519 is unavailable.
 */
export async function generateDeviceKeys(): Promise<DeviceKeys> {
  const msg = await featureDetectX25519();
  if (msg) throw new Error(msg);

  // generateKey returns CryptoKeyPair for asymmetric algorithms; TS needs the cast.
  const x25519 = await crypto.subtle.generateKey(
    { name: "X25519" },
    false, // non-extractable private key
    ["deriveBits"],
  ) as CryptoKeyPair;

  const ecdsa = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    false, // non-extractable private key
    ["sign"],
  ) as CryptoKeyPair;

  return {
    x25519Private: x25519.privateKey,
    x25519Public: x25519.publicKey,
    ecdsaPrivate: ecdsa.privateKey,
    ecdsaPublic: ecdsa.publicKey,
  };
}

/**
 * deriveDeviceKeysFromSeed derives both keypairs deterministically from a
 * 64-byte BIP-39 seed (ceremony and recovery flows).
 *
 * Uses HKDF-Expand-only (matching Go's hkdf.Expand) with domain labels:
 *   - X25519: "spawnery/device/x25519/v1"
 *   - ECDSA-P256: "spawnery/device/ecdsa-p256/v1"
 *
 * The returned keys have extractable:false for the private halves. The seed
 * passes through zeroable ArrayBuffers; callers must zero it after this call.
 * Per WL1, mnemonic-derived keys are extractable-by-construction while in use.
 */
export async function deriveDeviceKeysFromSeed(seed: Uint8Array): Promise<DeviceKeys> {
  // X25519: HKDF-Expand to a 32-byte sub-seed, then run the DHKEM DeriveKeyPair
  // (RFC 9180 §7.1.2) to match Go's kemScheme.DeriveKeyPair(xSeed) in
  // internal/secrets/seal/device.go. Applying clamp(xSeed)·G directly would
  // produce a different keypair — the DHKEM step is mandatory.
  const xSeed = await hkdfExpand(seed, "spawnery/device/x25519/v1", 32);
  const xScalar = await dhkemDerivePrivateScalar(xSeed);

  const xPriv = await importX25519PrivateKey(xScalar, false);
  const xPub = await crypto.subtle.exportKey("raw", await getX25519PublicKey(xPriv, xScalar));

  const x25519Public = await crypto.subtle.importKey(
    "raw",
    xPub,
    { name: "X25519" },
    true,
    [],
  );

  // P256: derive 48 bytes, reduce into [1, N-1] matching Go's deriveP256.
  const p256Seed = await hkdfExpand(seed, "spawnery/device/ecdsa-p256/v1", 48);
  const scalar32 = reduceP256Scalar(p256Seed);
  const ecdsaPriv = await importP256PrivateKey(scalar32, false);
  const ecdsaPub = await getP256PublicKey(scalar32);

  return {
    x25519Private: xPriv,
    x25519Public: x25519Public,
    ecdsaPrivate: ecdsaPriv,
    ecdsaPublic: ecdsaPub,
  };
}

/**
 * deriveDeviceKeysFromMnemonic is the convenience wrapper used in ceremony
 * and recovery flows. It converts the mnemonic to a seed, derives keys, and
 * zeros the intermediate seed buffer.
 */
export async function deriveDeviceKeysFromMnemonic(
  mnemonic: string,
  passphrase = "",
): Promise<DeviceKeys> {
  const seed = await mnemonicToSeed(mnemonic, passphrase);
  try {
    return await deriveDeviceKeysFromSeed(seed);
  } finally {
    seed.fill(0);
  }
}

/** exportDeviceRef extracts the public DeviceRef for registration. */
export async function exportDeviceRef(keys: DeviceKeys): Promise<DeviceRef> {
  const x25519RawBuf = await crypto.subtle.exportKey("raw", keys.x25519Public);
  const x25519Pub = new Uint8Array(x25519RawBuf);

  // Export P-256 public key in uncompressed SEC1 format (0x04 || X || Y).
  const rawBuf = await crypto.subtle.exportKey("raw", keys.ecdsaPublic);
  const signPub = new Uint8Array(rawBuf); // WebCrypto raw export for ECDSA public key is uncompressed

  return { x25519Pub, signPub };
}

// ── IndexedDB key storage ─────────────────────────────────────────────────────

function openIDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(IDB_DB_NAME, IDB_VERSION);
    req.onupgradeneeded = (ev) => {
      const db = (ev.target as IDBOpenDBRequest).result;
      if (!db.objectStoreNames.contains(IDB_STORE)) {
        db.createObjectStore(IDB_STORE);
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbPut(db: IDBDatabase, key: string, value: unknown): Promise<void> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readwrite");
    const store = tx.objectStore(IDB_STORE);
    const req = store.put(value, key);
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
}

function idbGet<T>(db: IDBDatabase, key: string): Promise<T | undefined> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readonly");
    const store = tx.objectStore(IDB_STORE);
    const req = store.get(key);
    req.onsuccess = () => resolve(req.result as T | undefined);
    req.onerror = () => reject(req.error);
  });
}

function idbDelete(db: IDBDatabase, key: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(IDB_STORE, "readwrite");
    const store = tx.objectStore(IDB_STORE);
    const req = store.delete(key);
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
}

/**
 * storeDeviceKeys persists the device keypairs in IndexedDB.
 * Private keys are non-extractable; they are stored as CryptoKey objects
 * which IndexedDB serializes as structured-clone (the keys never leave the
 * browser's secure key store in serializable form).
 *
 * Also calls navigator.storage.persist() and returns the granted status (WM11).
 */
export async function storeDeviceKeys(
  keys: DeviceKeys,
): Promise<{ persistGranted: boolean }> {
  const db = await openIDB();
  await Promise.all([
    idbPut(db, IDB_X25519_KEY, keys.x25519Private),
    idbPut(db, IDB_X25519_PUB_KEY, keys.x25519Public),
    idbPut(db, IDB_ECDSA_KEY, keys.ecdsaPrivate),
    idbPut(db, IDB_ECDSA_PUB_KEY, keys.ecdsaPublic),
  ]);
  db.close();

  let persistGranted = false;
  if (navigator.storage?.persist) {
    persistGranted = await navigator.storage.persist();
  }
  return { persistGranted };
}

/**
 * loadDeviceKeys retrieves the device keypairs from IndexedDB.
 * Returns null if no keys are stored (unenrolled state or key loss).
 */
export async function loadDeviceKeys(): Promise<DeviceKeys | null> {
  const db = await openIDB();
  const [xPriv, xPub, ePriv, ePub] = await Promise.all([
    idbGet<CryptoKey>(db, IDB_X25519_KEY),
    idbGet<CryptoKey>(db, IDB_X25519_PUB_KEY),
    idbGet<CryptoKey>(db, IDB_ECDSA_KEY),
    idbGet<CryptoKey>(db, IDB_ECDSA_PUB_KEY),
  ]);
  db.close();
  if (!xPriv || !xPub || !ePriv || !ePub) return null;
  return {
    x25519Private: xPriv,
    x25519Public: xPub,
    ecdsaPrivate: ePriv,
    ecdsaPublic: ePub,
  };
}

/**
 * clearDeviceKeys removes all stored device keys from IndexedDB.
 * Used when revocation is detected or recovery rotation is complete.
 */
export async function clearDeviceKeys(): Promise<void> {
  const db = await openIDB();
  await Promise.all([
    idbDelete(db, IDB_X25519_KEY),
    idbDelete(db, IDB_X25519_PUB_KEY),
    idbDelete(db, IDB_ECDSA_KEY),
    idbDelete(db, IDB_ECDSA_PUB_KEY),
  ]);
  db.close();
}

/**
 * isEnrolled returns true if device keys are present in IndexedDB.
 * A false result after prior enrollment indicates key loss (e.g., Safari
 * ITP eviction) — callers should prompt re-enroll + revoke-stale-member (WM11).
 */
export async function isEnrolled(): Promise<boolean> {
  const keys = await loadDeviceKeys();
  return keys !== null;
}

// ── Internal crypto helpers ───────────────────────────────────────────────────

/**
 * importX25519PrivateKey imports a raw 32-byte scalar as a non-extractable
 * X25519 private key using PKCS#8.
 */
async function importX25519PrivateKey(
  scalar32: Uint8Array,
  extractable: boolean,
): Promise<CryptoKey> {
  // PKCS#8 for X25519: RFC 8410, OID 1.3.101.110
  // SEQUENCE { INTEGER 0, SEQUENCE { OID 1.3.101.110 }, OCTET STRING { OCTET STRING scalar } }
  // Total: 48 bytes
  const prefix = new Uint8Array([
    0x30, 0x2e, // SEQUENCE, length 46
    0x02, 0x01, 0x00, // INTEGER(0) version
    0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, // SEQUENCE { OID 1.3.101.110 }
    0x04, 0x22, 0x04, 0x20, // OCTET STRING { OCTET STRING, length 32 }
  ]);
  const pkcs8 = new Uint8Array(prefix.length + 32);
  pkcs8.set(prefix);
  pkcs8.set(scalar32, prefix.length);
  return crypto.subtle.importKey("pkcs8", pkcs8, { name: "X25519" }, extractable, [
    "deriveBits",
  ]);
}

/**
 * getX25519PublicKey derives the public key from a private key by performing
 * a deriveBits against the standard base point. WebCrypto does not expose a
 * direct public-key-from-private API for X25519, so we export the public
 * component another way.
 *
 * We use the fact that when importing a raw X25519 public key we can also
 * get the public part from the PKCS8 structure by re-importing the scalar
 * as extractable and then exporting (for seed-derived keys only, during ceremony).
 */
async function getX25519PublicKey(
  _priv: CryptoKey,
  seed: Uint8Array,
): Promise<CryptoKey> {
  // Import as extractable to get the public key, then import the public key
  // as non-extractable. (Used only during ceremony/recovery with seed material.)
  const extractablePriv = await importX25519PrivateKey(seed, true);
  // Use JWK export to get the x component (the 32-byte raw public key).
  const jwk = await crypto.subtle.exportKey("jwk", extractablePriv);
  if (!jwk.x) throw new Error("X25519: JWK export missing x (public key)");
  const pubBytes = base64urlToBytes(jwk.x);
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  return crypto.subtle.importKey("raw", pubBytes as unknown as Uint8Array<ArrayBuffer>, { name: "X25519" }, true, []);
}

/**
 * importP256PrivateKey imports a 32-byte P-256 scalar as a PKCS#8 private key.
 * The PKCS#8 structure is hardcoded for secp256r1.
 */
async function importP256PrivateKey(
  scalar32: Uint8Array,
  extractable: boolean,
): Promise<CryptoKey> {
  const pkcs8 = new Uint8Array(PKCS8_P256_PREFIX.length + 32);
  pkcs8.set(PKCS8_P256_PREFIX);
  pkcs8.set(scalar32, PKCS8_P256_PREFIX.length);
  return crypto.subtle.importKey(
    "pkcs8",
    pkcs8,
    { name: "ECDSA", namedCurve: "P-256" },
    extractable,
    ["sign"],
  );
}

/**
 * getP256PublicKey derives the P-256 public key from a raw 32-byte scalar by
 * importing it as extractable, reading x/y via JWK, then returning the public
 * key. This mirrors getX25519PublicKey and avoids requiring an extractable
 * private CryptoKey (non-extractable keys cannot be JWK-exported).
 */
async function getP256PublicKey(scalar32: Uint8Array): Promise<CryptoKey> {
  const extractablePriv = await importP256PrivateKey(scalar32, true);
  const jwk = await crypto.subtle.exportKey("jwk", extractablePriv);
  const pubJwk: JsonWebKey = { kty: "EC", crv: "P-256", x: jwk.x, y: jwk.y };
  return crypto.subtle.importKey(
    "jwk",
    pubJwk,
    { name: "ECDSA", namedCurve: "P-256" },
    true,
    ["verify"],
  );
}

/**
 * reduceP256Scalar reduces a 48-byte buffer to a P-256 scalar in [1, N-1]
 * matching Go's deriveP256: d = (buf mod (N-1)) + 1.
 *
 * P-256 order N = FFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551
 */
export function reduceP256Scalar(buf48: Uint8Array): Uint8Array {
  if (buf48.length !== 48) throw new Error("reduceP256Scalar: expected 48 bytes");
  const N = BigInt(
    "0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551",
  );
  const N1 = N - 1n;

  // Convert buf to BigInt (big-endian)
  let d = BigInt(0);
  for (const b of buf48) d = (d << 8n) | BigInt(b);

  d = d % N1;
  d = d + 1n; // map to [1, N-1]

  // Serialize to 32 bytes big-endian
  const out = new Uint8Array(32);
  for (let i = 31; i >= 0; i--) {
    out[i] = Number(d & 0xffn);
    d >>= 8n;
  }
  return out;
}

/** base64urlToBytes decodes a base64url string (no padding) to Uint8Array. */
function base64urlToBytes(b64url: string): Uint8Array {
  const b64 = b64url.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
