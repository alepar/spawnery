/**
 * Proof-of-Possession (PoP) construction for /refresh [AM5].
 *
 * Message = "spawnery/refresh-pop/v1" || refreshTokenHash(32B) || be64(timestamp) || nonce(16B)
 * Signature = ECDSA P-256 P1363 over SHA-256(message)
 *
 * Headers emitted:
 *   X-PoP-Timestamp: <decimal unix s>
 *   X-PoP-Nonce:     <base64url-unpadded 16 random bytes>
 *   X-PoP-Sig:       <base64url-unpadded P1363 64 bytes>
 *
 * refreshTokenHash: SHA-256 of the raw refresh token string.
 * The SPA cannot read the HttpOnly cookie, so the server returns this hash in
 * the callback redirect (?refresh_token_hash=) and in each /refresh JSON response.
 * Both sides use the SAME hash (R1 resolution).
 */

import { signP1363 } from "./keypair";
import { toBase64Url } from "./token";

const POP_DOMAIN = "spawnery/refresh-pop/v1";

export interface PoPHeaders {
  "X-PoP-Timestamp": string;
  "X-PoP-Nonce": string;
  "X-PoP-Sig": string;
}

/**
 * buildPoP signs the PoP message and returns the three header values.
 *
 * @param privateKey  - Non-extractable ECDSA P-256 session private key.
 * @param refreshTokenHash - 32-byte SHA-256 of the raw refresh token (from AS).
 * @param now         - Current time (injectable for tests).
 * @param nonce       - Optional 16-byte nonce (injectable for tests; random if omitted).
 */
export async function buildPoP(
  privateKey: CryptoKey,
  refreshTokenHash: Uint8Array,
  now: Date = new Date(),
  nonce?: Uint8Array,
): Promise<PoPHeaders> {
  const ts = Math.floor(now.getTime() / 1000);

  const nonceBytes = nonce ?? crypto.getRandomValues(new Uint8Array(16));

  // Build signed message: domain || refreshTokenHash || be64(ts) || nonce
  const domainBytes = new TextEncoder().encode(POP_DOMAIN);
  const tsBytes = new Uint8Array(8);
  new DataView(tsBytes.buffer).setBigUint64(0, BigInt(ts), false); // big-endian

  const msg = new Uint8Array(
    domainBytes.length + refreshTokenHash.length + 8 + nonceBytes.length,
  );
  let off = 0;
  msg.set(domainBytes, off); off += domainBytes.length;
  msg.set(refreshTokenHash, off); off += refreshTokenHash.length;
  msg.set(tsBytes, off); off += 8;
  msg.set(nonceBytes, off);

  const sig = await signP1363(privateKey, msg);

  return {
    "X-PoP-Timestamp": String(ts),
    "X-PoP-Nonce": toBase64Url(nonceBytes),
    "X-PoP-Sig": toBase64Url(sig),
  };
}
