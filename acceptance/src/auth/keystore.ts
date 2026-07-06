/**
 * Node-side KeyStore implementations for @spawnery/client's SpawnClient (design §Auth). The dev CP
 * mints the node token straight from the intent's own SPKI (no separate pubkey registration), so a
 * freshly generated, never-persisted keypair verifies out of the box — NodeMemoryKeyStore covers
 * every dev-token identity. OAuth-PoP is different: the CP binds the intent to the session's own
 * cnf pubkey (see oauth-session.ts's session_pubkey), so that flow must hand SpawnClient the SAME
 * keypair that signs PoP refreshes — keyPairKeyStore wraps an already-established SessionKeyPair
 * for that case (see oauthpop.ts's sessionKeyStore).
 */

import type { KeyStore, SessionKeyPair } from "@spawnery/client";

/** NodeMemoryKeyStore lazily generates one non-extractable ECDSA P-256 keypair on first get() and
 * caches it for the lifetime of the process (mirrors oauth-session.ts's generateSessionKey). */
export class NodeMemoryKeyStore implements KeyStore {
  private kp?: SessionKeyPair;

  async get(): Promise<SessionKeyPair | null> {
    if (!this.kp) {
      const { privateKey, publicKey } = (await crypto.subtle.generateKey(
        { name: "ECDSA", namedCurve: "P-256" },
        false,
        ["sign", "verify"],
      )) as CryptoKeyPair;
      this.kp = { privateKey, publicKey };
    }
    return this.kp;
  }

  async put(kp: SessionKeyPair): Promise<void> {
    this.kp = kp;
  }

  async delete(): Promise<void> {
    this.kp = undefined;
  }
}

/** keyPairKeyStore wraps an already-established SessionKeyPair as a KeyStore — used to hand
 * SpawnClient the OAuth-PoP session's own cnf-bound key rather than generating a fresh one. */
export function keyPairKeyStore(kp: SessionKeyPair): KeyStore {
  let current: SessionKeyPair | null = kp;
  return {
    async get() {
      return current;
    },
    async put(next: SessionKeyPair) {
      current = next;
    },
    async delete() {
      current = null;
    },
  };
}
