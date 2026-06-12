/**
 * SAS (Short Authentication String) derivation for enrollment (Phase 5, [WM4]).
 *
 * Both sides independently derive the verification code from:
 *   SHA-256(encodeFields("sas/v1", genesis_hash, head_hash, new_x25519_pub, new_sign_pub))
 *
 * The code is NEVER parsed from the enrollment link — a code carried in the
 * link authenticates nothing ([WM4]). Both sides compute it independently
 * from their own view of the data; the approver from the pubkeys it received
 * in the link, the enrollee from its own generated keys.
 *
 * Encoding: first 6 bytes of the SHA-256 output encoded as base-36 (lowercase
 * alphanum), chunked into 3×4 characters for readability.
 * Entropy: 48 bits, exceeds the ≥48-bit spec requirement.
 */

import { encodeFields, sha256 } from "./encoding";

const SAS_VERSION = new TextEncoder().encode("sas/v1");

/**
 * deriveSAS computes the enrollment SAS code from the chain anchors and the
 * new device's public keys.
 *
 * Returns a 12-character human-comparable string (3 groups of 4, e.g. "a4bx-r2zy-8t3k").
 */
export async function deriveSAS(
  genesisHash: Uint8Array,
  headHash: Uint8Array,
  newX25519Pub: Uint8Array,
  newSignPub: Uint8Array,
): Promise<string> {
  const digest = await sha256(
    encodeFields(SAS_VERSION, genesisHash, headHash, newX25519Pub, newSignPub),
  );
  // Take first 6 bytes (48 bits) and encode as base-36
  const n = (
    BigInt(digest[0]) << 40n |
    BigInt(digest[1]) << 32n |
    BigInt(digest[2]) << 24n |
    BigInt(digest[3]) << 16n |
    BigInt(digest[4]) << 8n |
    BigInt(digest[5])
  );
  // Encode as at least 10-char base-36 string, left-padded with 'a' (char 10)
  const BASE = 36n;
  const chars = "0123456789abcdefghijklmnopqrstuvwxyz";
  let val = n;
  let code = "";
  for (let i = 0; i < 12; i++) {
    code = chars[Number(val % BASE)] + code;
    val /= BASE;
  }
  // Split into 3 groups of 4
  return `${code.slice(0, 4)}-${code.slice(4, 8)}-${code.slice(8, 12)}`;
}

/**
 * verifyEnrollmentSAS checks that the SAS the approver computed matches the
 * SAS the user compared out-of-band. Constant-time comparison is not required
 * here (the SAS is displayed to both humans — timing is irrelevant), but we
 * keep a strict equality check.
 */
export function verifySAS(computed: string, displayed: string): boolean {
  return computed === displayed;
}
