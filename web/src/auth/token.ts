/**
 * Session access-token wire format (auth-identity design §3 [MC1]).
 *
 * Wire: base64url(body_bytes) "." base64url(sig_bytes) — RawURLEncoding (no padding).
 * Body = proto3 SessionTokenBody. The SPA only READS the body; the AS signs it (Ed25519).
 * The SPA does NOT verify the Ed25519 sig — the CP verifies on every RPC (MC2).
 *
 * Fields read by the SPA:
 *   f1 account_id (string)
 *   f2 handle     (string)
 *   f6 expires_at (int64 unix s)  — schedule refresh
 *   f7 session_key_hash (bytes)   — cnf check against local SPKI
 */

import { readFields } from "./protobuf";

// ── Base64url helpers (unpadded, RawURLEncoding) ──────────────────────────────

/** Decode a RawURLEncoding base64url string to Uint8Array (no padding required). */
export function fromBase64Url(s: string): Uint8Array {
  // Normalize: replace URL-safe chars and add padding.
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Encode bytes to RawURLEncoding base64url (no padding). */
export function toBase64Url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, "");
}

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
 * Unrecognized fields are ignored.
 */
export function decodeSessionTokenBody(bodyBytes: Uint8Array): SessionTokenBodyDecoded {
  const fields = readFields(bodyBytes);
  let accountId = "";
  let handle = "";
  let expiresAt = 0n;
  let sessionKeyHash = new Uint8Array(0);

  for (const f of fields) {
    switch (f.fieldNumber) {
      case 1: // account_id (string)
        if (f.bytes) accountId = new TextDecoder().decode(f.bytes);
        break;
      case 2: // handle (string)
        if (f.bytes) handle = new TextDecoder().decode(f.bytes);
        break;
      case 6: // expires_at (int64)
        if (f.varint !== undefined) expiresAt = f.varint;
        break;
      case 7: // session_key_hash (bytes)
        if (f.bytes) sessionKeyHash = f.bytes.slice();
        break;
    }
  }
  return { accountId, handle, expiresAt, sessionKeyHash };
}

/** parseAccessToken is a convenience wrapper over parseTokenWire + decodeSessionTokenBody. */
export function parseAccessToken(wire: string): SessionTokenBodyDecoded & { bodyBytes: Uint8Array } {
  const { bodyBytes } = parseTokenWire(wire);
  return { ...decodeSessionTokenBody(bodyBytes), bodyBytes };
}
