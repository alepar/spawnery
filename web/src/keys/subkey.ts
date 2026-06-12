/**
 * Node HPKE sub-key: TypeScript mirror of internal/secrets/subkey (Go).
 *
 * Implements:
 *   - SignedSubKey — the JSON structure published by a node
 *   - verifySignedSubKey — ECDSA-P256 sig check + expiry check
 *   - verifyNodeForSealing — full verification chain (cert + sub-key) returning
 *     a trusted HPKE pubkey for use with seal/reseal operations
 *
 * Design: docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §1 §3
 *
 * Node cert profile: P-256 leaf, SAN = <nodeId>.<accountId>.<class>.nodes.spawnery.internal
 *
 * WM10: UnixNano timestamps are int64 — must use BigInt in encodeFields.
 */

import { derToP1363 } from "./der";
import {
  verifyCertChain,
  importCertPubKey,
  parseSANIdentity,
  ParsedCert,
} from "./x509";

// ── Encoding helpers (mirror of subkey.go + seal.go encodeFields) ────────────

const enc = new TextEncoder();

function encodeFields(...parts: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const p of parts) total += 8 + p.length;
  const out = new Uint8Array(total);
  let off = 0;
  const view = new DataView(out.buffer);
  for (const p of parts) {
    view.setBigUint64(off, BigInt(p.length), false);
    off += 8;
    out.set(p, off);
    off += p.length;
  }
  return out;
}

/** Encode a BigInt as 8-byte big-endian. Must use BigInt for UnixNano (WM10). */
function u64big(v: bigint): Uint8Array {
  const b = new Uint8Array(8);
  new DataView(b.buffer).setBigUint64(0, v, false);
  return b;
}

/** Helpers to convert from base64. */
function fromBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// ── SignedSubKey ──────────────────────────────────────────────────────────────

/** Mirror of subkey.go SignedSubKey (JSON-deserialized from the CP). */
export interface SignedSubKey {
  hpke_pub:   string; // base64 raw 32-byte X25519 pubkey
  node_id:    string;
  not_before: string; // RFC 3339 / ISO 8601
  not_after:  string; // RFC 3339 / ISO 8601
  sig:        string; // base64 ASN.1 DER ECDSA-P256 signature
}

// sigDomain must match Go's subkey.go const sigDomain.
const sigDomain = "spawnery/secrets/subkey/v1";

/**
 * Compute the canonical byte string that the sub-key signature covers.
 * Mirror of SignedSubKey.signedBytes() in Go.
 *
 * encodeFields(sigDomain, HPKEPub, NodeID, u64(NotBefore.UnixNano), u64(NotAfter.UnixNano))
 */
function signedBytes(sk: SignedSubKey): Uint8Array {
  const hpkePub   = fromBase64(sk.hpke_pub);
  const notBefore = BigInt(new Date(sk.not_before).getTime()) * 1_000_000n; // ms → ns
  const notAfter  = BigInt(new Date(sk.not_after).getTime()) * 1_000_000n;
  return encodeFields(
    enc.encode(sigDomain),
    hpkePub,
    enc.encode(sk.node_id),
    u64big(notBefore),
    u64big(notAfter),
  );
}

/**
 * Verify a SignedSubKey's ECDSA-P256 signature against certPub (the leaf cert
 * public key), and check that now is within [not_before, not_after).
 * Throws on any failure.
 */
export async function verifySignedSubKey(
  sk: SignedSubKey,
  certPub: CryptoKey,
  now: Date,
): Promise<void> {
  // Expiry check.
  const notBefore = new Date(sk.not_before);
  const notAfter  = new Date(sk.not_after);
  if (now < notBefore) throw new Error(`subkey: not yet valid (not_before=${sk.not_before})`);
  if (now >= notAfter)  throw new Error(`subkey: expired (not_after=${sk.not_after})`);

  // Signature check.
  const sigDER = fromBase64(sk.sig);
  const sigP1363 = derToP1363(sigDER);
  const msg = signedBytes(sk);
  const ok = await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    certPub,
    sigP1363.buffer.slice(sigP1363.byteOffset, sigP1363.byteOffset + sigP1363.byteLength) as ArrayBuffer,
    msg.buffer.slice(msg.byteOffset, msg.byteOffset + msg.byteLength) as ArrayBuffer,
  );
  if (!ok) throw new Error("subkey: signature does not verify against the node cert key");
}

// ── VerifyNodeForSealing ──────────────────────────────────────────────────────

/**
 * Verified node identity returned by verifyNodeForSealing.
 * Mirrors pki.Identity in Go.
 */
export interface NodeIdentity {
  nodeId:    string;
  accountId: string;
  nodeClass: string; // "cloud" | "self-hosted"
}

/**
 * Client-side sealing expectation (mirrors subkey.Expectation / clientverify.Expectation in Go).
 * tenancy = "cloud" | "self-hosted". For self-hosted, accountId is checked.
 */
export interface SealingExpectation {
  tenancy:   "cloud" | "self-hosted";
  accountId?: string;
}

/**
 * RevocationChecker is the injected hook for the AS-published node revocation/deny-list
 * (spec §3 step 2, prior-roast M12: revocation ≠ expiry).
 *
 * A node may re-sign fresh sub-keys with its own cert key indefinitely, so validity alone
 * does not revoke. Clients consult this checker at delivery step 2 and refuse to seal to a
 * revoked node. The checker MUST throw on any lookup error so callers fail closed.
 *
 * AllowAll is the default until the AS revocation-list endpoint is wired in.
 * Mirror of subkey/verify.go RevocationChecker interface.
 */
export interface RevocationChecker {
  /** Throw to block sealing (revoked node or lookup error). Return void to allow. */
  check(nodeId: string): Promise<void>;
}

/** AllowAll is the default RevocationChecker: permits every node. */
export const AllowAll: RevocationChecker = {
  async check(_nodeId: string): Promise<void> { /* allow all */ },
};

/**
 * Full verification chain before sealing (spec §3 step 2). In order:
 *
 *   1. node cert chains to pinned rootPEM + SAN matches expect
 *   2. AS revocation check, fail-closed (prior-roast M12) — throws on revoked or lookup error
 *   3. sub-key nodeID matches verified cert identity
 *   4. sub-key signature chains to cert key
 *   5. sub-key is unexpired
 *
 * Returns the trusted HPKE X25519 pubkey (raw 32 bytes) and the verified node
 * identity, or throws on any failure.
 *
 * certChainPEM is the CP-relayed leaf+chain PEM; rootPEM is the client-pinned
 * Root CA. If certChainPEM is empty (dev/insecure mode), step 1 is skipped and
 * the sub-key pubkey is returned trusted with a synthetic "dev" identity.
 *
 * subkeyJSON is the raw JSON string from GetSpawnNodeKeyResponse.signed_subkey.
 * now is the current time (injectable for testing).
 * revocationChecker defaults to AllowAll if not supplied.
 */
export async function verifyNodeForSealing(
  certChainPEM: string,
  rootPEM: string,
  subkeyJSON: string,
  expect: SealingExpectation,
  now: Date,
  revocationChecker?: RevocationChecker,
): Promise<{ hpkePub: Uint8Array; identity: NodeIdentity }> {
  const sk: SignedSubKey = JSON.parse(subkeyJSON);

  // Dev/insecure mode: no cert chain → skip chain verification.
  if (!certChainPEM) {
    return {
      hpkePub:  fromBase64(sk.hpke_pub),
      identity: { nodeId: sk.node_id, accountId: "dev", nodeClass: "cloud" },
    };
  }

  // 1. Verify cert chain against pinned root + extract identity from SAN.
  // now is passed through so the cert validity window is checked with the same clock as the sub-key.
  const leaf = await verifyCertChain(certChainPEM, rootPEM, now);
  const identity = parseSANIdentity(leaf.sanDNS);

  // Tenancy check.
  if (identity.nodeClass !== expect.tenancy) {
    throw new Error(`subkey: expected tenancy ${JSON.stringify(expect.tenancy)}, got ${JSON.stringify(identity.nodeClass)}`);
  }
  if (expect.tenancy === "self-hosted") {
    if (!expect.accountId) throw new Error("subkey: accountId required for self-hosted expectation");
    if (identity.accountId !== expect.accountId) {
      throw new Error(`subkey: self-hosted node bound to ${JSON.stringify(identity.accountId)}, not ${JSON.stringify(expect.accountId)}`);
    }
  }

  // 2. AS revocation check, fail-closed (prior-roast M12).
  // Any error from the checker (network, parse, revoked) propagates as-is and blocks sealing.
  const checker = revocationChecker ?? AllowAll;
  await checker.check(identity.nodeId);

  // 3. Sub-key nodeID must match verified cert identity.
  if (sk.node_id !== identity.nodeId) {
    throw new Error(`subkey: sub-key node_id=${JSON.stringify(sk.node_id)} != cert nodeId=${JSON.stringify(identity.nodeId)}`);
  }

  // 4 + 5. Verify sub-key signature + expiry.
  const certPub = await importCertPubKey(leaf);
  await verifySignedSubKey(sk, certPub, now);

  return { hpkePub: fromBase64(sk.hpke_pub), identity };
}

// Export ParsedCert for callers that want the leaf cert separately.
export type { ParsedCert };
