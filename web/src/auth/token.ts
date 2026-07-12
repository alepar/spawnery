/**
 * Session access-token wire format (auth-identity design §3 [MC1]).
 *
 * Wire: one unpadded base64url SignedAuthArtifact protobuf envelope. The SPA decodes envelope
 * metadata and the exact SessionTokenBody payload. It does not verify certificates or signatures;
 * the CP performs root-anchored verification on every RPC.
 *
 * Fields read by the SPA:
 *   f1 account_id (string)
 *   f2 handle     (string)
 *   f6 expires_at (int64 unix s)  — schedule refresh
 *   f7 session_key_hash (bytes)   — cnf check against local SPKI
 */

import { fromBinary } from "@bufbuild/protobuf";
import { authv1, toBase64Url, fromBase64Url } from "@spawnery/client";

export { toBase64Url, fromBase64Url };

// ── Wire parsing ──────────────────────────────────────────────────────────────

export interface TokenParts {
  artifactType: string;
  payloadBytes: Uint8Array;
  signatureBytes: Uint8Array;
  signerChain: Uint8Array[];
  keyId: Uint8Array;
}

/**
 * parseTokenWire decodes the self-describing envelope without treating its metadata as trusted.
 */
export function parseTokenWire(wire: string): TokenParts {
  if (wire.length === 0 || wire.includes(".")) throw new Error("token: malformed envelope");
  let artifact;
  try {
    artifact = fromBinary(authv1.SignedAuthArtifactSchema, fromBase64Url(wire));
  } catch {
    throw new Error("token: malformed envelope");
  }
  if (artifact.artifactType !== "session-token") {
    throw new Error(`token: unexpected artifact type ${JSON.stringify(artifact.artifactType)}`);
  }
  if (artifact.payload.length === 0 || artifact.signature.length !== 64 ||
      artifact.signerChain.length === 0 || artifact.keyId.length !== 32) {
    throw new Error("token: malformed envelope");
  }
  return {
    artifactType: artifact.artifactType,
    payloadBytes: artifact.payload,
    signatureBytes: artifact.signature,
    signerChain: artifact.signerChain,
    keyId: artifact.keyId,
  };
}

// ── SessionTokenBody fields ───────────────────────────────────────────────────

export interface SessionTokenBodyDecoded {
  accountId: string;
  handle: string;
  expiresAt: bigint; // unix seconds as BigInt (WM10: avoid float precision loss)
  sessionKeyHash: Uint8Array; // 32-byte SHA-256 of DER SPKI
}

/**
 * decodeSessionTokenBody parses the proto3 body bytes into the fields the SPA uses.
 * Unrecognized fields are ignored (protobuf-es fromBinary default behavior).
 */
export function decodeSessionTokenBody(bodyBytes: Uint8Array): SessionTokenBodyDecoded {
  const body = fromBinary(authv1.SessionTokenBodySchema, bodyBytes);
  return {
    accountId: body.accountId,
    handle: body.handle,
    expiresAt: body.expiresAt,
    sessionKeyHash: body.sessionKeyHash,
  };
}

/** parseAccessToken decodes the session body from the envelope's exact payload bytes. */
export function parseAccessToken(wire: string): SessionTokenBodyDecoded & { bodyBytes: Uint8Array } {
  const { payloadBytes } = parseTokenWire(wire);
  return { ...decodeSessionTokenBody(payloadBytes), bodyBytes: payloadBytes };
}
