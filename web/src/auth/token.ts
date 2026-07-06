/**
 * Session access-token wire format (auth-identity design §3 [MC1]).
 *
 * Wire: base64url(body_bytes) "." base64url(sig_bytes) — RawURLEncoding (no padding).
 * Body = proto3 SessionTokenBody, decoded with protobuf-es (via @spawnery/client's generated
 * gen/auth/v1/auth_pb). The SPA only READS the body; the AS signs it (Ed25519).
 * The SPA does NOT verify the Ed25519 sig — the CP verifies on every RPC (MC2).
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
  bodyBytes: Uint8Array;
  sigBytes: Uint8Array;
}

/**
 * parseTokenWire splits the wire token "base64url(body).base64url(sig)".
 * Does NOT verify the sig — that is the CP's job (MC2).
 */
export function parseTokenWire(wire: string): TokenParts {
  const dot = wire.indexOf(".");
  if (dot < 0) throw new Error("token: malformed wire (no dot)");
  return {
    bodyBytes: fromBase64Url(wire.slice(0, dot)),
    sigBytes: fromBase64Url(wire.slice(dot + 1)),
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

/** parseAccessToken is a convenience wrapper over parseTokenWire + decodeSessionTokenBody. */
export function parseAccessToken(wire: string): SessionTokenBodyDecoded & { bodyBytes: Uint8Array } {
  const { bodyBytes } = parseTokenWire(wire);
  return { ...decodeSessionTokenBody(bodyBytes), bodyBytes };
}
