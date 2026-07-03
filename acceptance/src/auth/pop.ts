/**
 * Proof-of-Possession (PoP) construction for POST /refresh, reproduced in Node for the
 * OAuthPoPAuth oracle (design §Auth; Spike S1 result, approach (b)). This is a byte-for-byte port
 * of web/src/auth/pop.ts — duplicated, not imported: acceptance/ is not a dependency of web/ (see
 * acceptance/src/auth/devtoken.ts's header for the same convention). The wire contract is frozen
 * and shared across three implementations (Go: cmd/spawnctl/authstate.go,
 * internal/authsvc/pop.go; browser: web/src/auth/pop.ts; this file):
 *
 * Message   = "spawnery/refresh-pop/v1" || sha256(refresh_token_bytes)(32B) || be64(ts) || nonce(16B)
 * Signature = ECDSA P-256, P1363 raw 64-byte r||s, over SHA-256(message)
 *
 * Node's global WebCrypto (crypto.subtle, available unprefixed since Node 20 — see
 * acceptance/package.json's engines.node) produces ECDSA signatures in the same raw P1363 format
 * as the browser's SubtleCrypto, so this needs no format conversion (verified: internal/authsvc's
 * VerifyPoP treats the 64-byte signature as raw r||s, same as web/src/auth/pop.ts's WebCrypto call).
 *
 * Headers emitted:
 *   X-PoP-Timestamp: <decimal unix s>
 *   X-PoP-Nonce:     <base64url-unpadded 16 random bytes>
 *   X-PoP-Sig:       <base64url-unpadded P1363 64 bytes>
 */

const POP_DOMAIN = "spawnery/refresh-pop/v1";

export interface PoPHeaders {
  "X-PoP-Timestamp": string;
  "X-PoP-Nonce": string;
  "X-PoP-Sig": string;
}

/** toBase64Url encodes bytes as unpadded base64url (RawURLEncoding — matches the Go/browser wire). */
export function toBase64Url(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString("base64url");
}

/** fromBase64Url decodes unpadded base64url bytes (RawURLEncoding). */
export function fromBase64Url(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, "base64url"));
}

/** sha256 returns SHA-256(data). */
export async function sha256(data: Uint8Array): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", data as BufferSource));
}

/**
 * buildPoP signs the PoP message and returns the three header values.
 *
 * @param privateKey       - ECDSA P-256 session private key (see oauth-session.ts).
 * @param refreshTokenHash - 32-byte SHA-256 of the raw refresh token (as returned by the AS).
 * @param now              - Current time (injectable for tests).
 * @param nonce             - Optional 16-byte nonce (injectable for tests; random if omitted).
 */
export async function buildPoP(
  privateKey: CryptoKey,
  refreshTokenHash: Uint8Array,
  now: Date = new Date(),
  nonce?: Uint8Array,
): Promise<PoPHeaders> {
  const ts = Math.floor(now.getTime() / 1000);
  const nonceBytes = nonce ?? crypto.getRandomValues(new Uint8Array(16));

  const domainBytes = new TextEncoder().encode(POP_DOMAIN);
  const tsBytes = new Uint8Array(8);
  new DataView(tsBytes.buffer).setBigUint64(0, BigInt(ts), false); // big-endian

  const msg = new Uint8Array(domainBytes.length + refreshTokenHash.length + 8 + nonceBytes.length);
  let off = 0;
  msg.set(domainBytes, off); off += domainBytes.length;
  msg.set(refreshTokenHash, off); off += refreshTokenHash.length;
  msg.set(tsBytes, off); off += 8;
  msg.set(nonceBytes, off);

  const sig = new Uint8Array(
    await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, msg as BufferSource),
  );

  return {
    "X-PoP-Timestamp": String(ts),
    "X-PoP-Nonce": toBase64Url(nonceBytes),
    "X-PoP-Sig": toBase64Url(sig),
  };
}
