/**
 * KeyStore abstraction for the session keypair (ECDSA P-256, non-extractable).
 *
 * Implementations (browser IndexedDB-backed, Node in-memory, …) are app-side —
 * the SDK only defines the interface and signs with whatever keypair a KeyStore
 * hands it.
 */

export interface SessionKeyPair {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
}

export interface KeyStore {
  get(): Promise<SessionKeyPair | null>;
  put(kp: SessionKeyPair): Promise<void>;
  delete(): Promise<void>;
}
