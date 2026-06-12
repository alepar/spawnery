/**
 * KeyStore abstraction for the session keypair (ECDSA P-256, non-extractable).
 *
 * Keeps the session key in a SEPARATE object store from the device key
 * (internal/keys/device.ts uses "spawnery-device-keys"). This way a session-key
 * rotation never touches device keys and vice versa.
 *
 * The in-memory implementation is exported so unit tests never touch real IndexedDB
 * (structured-clone of non-extractable CryptoKey is unreliable in Node; keep fakes).
 */

const IDB_DB_NAME = "spawnery-auth";
const IDB_STORE = "session-key";
const IDB_VERSION = 1;
const KEY_PRIVATE = "private";
const KEY_PUBLIC = "public";

export interface SessionKeyPair {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
}

export interface KeyStore {
  get(): Promise<SessionKeyPair | null>;
  put(kp: SessionKeyPair): Promise<void>;
  delete(): Promise<void>;
}

// ── IndexedDB implementation ──────────────────────────────────────────────────

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

/** IndexedDB-backed KeyStore (production). Non-extractable CryptoKeys are stored as structured-clone. */
export class IDBKeyStore implements KeyStore {
  async get(): Promise<SessionKeyPair | null> {
    const db = await openIDB();
    const [priv, pub] = await Promise.all([
      idbGet<CryptoKey>(db, KEY_PRIVATE),
      idbGet<CryptoKey>(db, KEY_PUBLIC),
    ]);
    db.close();
    if (!priv || !pub) return null;
    return { privateKey: priv, publicKey: pub };
  }

  async put(kp: SessionKeyPair): Promise<void> {
    const db = await openIDB();
    await Promise.all([
      idbPut(db, KEY_PRIVATE, kp.privateKey),
      idbPut(db, KEY_PUBLIC, kp.publicKey),
    ]);
    db.close();
  }

  async delete(): Promise<void> {
    const db = await openIDB();
    await Promise.all([
      idbDelete(db, KEY_PRIVATE),
      idbDelete(db, KEY_PUBLIC),
    ]);
    db.close();
  }
}

// ── In-memory implementation (tests) ─────────────────────────────────────────

/** In-memory KeyStore for unit tests. Holds CryptoKey objects directly (no structured-clone). */
export class MemoryKeyStore implements KeyStore {
  private stored: SessionKeyPair | null = null;

  async get(): Promise<SessionKeyPair | null> {
    return this.stored ? { ...this.stored } : null;
  }

  async put(kp: SessionKeyPair): Promise<void> {
    this.stored = { ...kp };
  }

  async delete(): Promise<void> {
    this.stored = null;
  }
}
