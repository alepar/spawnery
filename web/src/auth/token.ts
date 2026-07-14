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
import { bytesEqual } from "@/keys/encoding";

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
  tokenId: string;
  audience: string;
  familyId: string;
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
    tokenId: body.tokenId,
    audience: body.audience,
    familyId: body.familyId,
    expiresAt: body.expiresAt,
    sessionKeyHash: body.sessionKeyHash,
  };
}

/** parseAccessToken decodes the session body from the envelope's exact payload bytes. */
export function parseAccessToken(wire: string): SessionTokenBodyDecoded & { bodyBytes: Uint8Array } {
  const { payloadBytes } = parseTokenWire(wire);
  return { ...decodeSessionTokenBody(payloadBytes), bodyBytes: payloadBytes };
}

export interface AccessTokenPair {
  cpAccessToken: string;
  nodeAccessToken: string;
}

export interface ValidatedAccessTokenPair {
  pair: AccessTokenPair;
  cp: SessionTokenBodyDecoded;
  node: SessionTokenBodyDecoded;
  accountId: string;
  expiresAt: bigint;
}

/** Validate the paired credentials before either credential enters application state. */
export function validateAccessTokenPair(
  pair: AccessTokenPair,
  localSpkiHash?: Uint8Array,
): ValidatedAccessTokenPair {
  if (!pair.cpAccessToken || !pair.nodeAccessToken) throw new Error("token pair: incomplete");
  const cp = parseAccessToken(pair.cpAccessToken);
  const node = parseAccessToken(pair.nodeAccessToken);
  if (cp.audience !== "cp" || node.audience !== "node") {
    throw new Error("token pair: invalid audiences");
  }
  if (!cp.accountId || cp.accountId !== node.accountId) throw new Error("token pair: account mismatch");
  if (!cp.familyId || cp.familyId !== node.familyId) throw new Error("token pair: family mismatch");
  if (!cp.tokenId || !node.tokenId || cp.tokenId === node.tokenId) {
    throw new Error("token pair: invalid token IDs");
  }
  if (cp.expiresAt <= 0n || cp.expiresAt !== node.expiresAt) throw new Error("token pair: expiry mismatch");
  if (cp.sessionKeyHash.length !== 32 || node.sessionKeyHash.length !== 32 ||
      !bytesEqual(cp.sessionKeyHash, node.sessionKeyHash)) {
    throw new Error("token pair: session key mismatch");
  }
  if (localSpkiHash && (localSpkiHash.length !== 32 || !bytesEqual(cp.sessionKeyHash, localSpkiHash))) {
    throw new Error("token pair: local session key mismatch");
  }
  return { pair, cp, node, accountId: cp.accountId, expiresAt: cp.expiresAt };
}
