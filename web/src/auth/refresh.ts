/**
 * Silent /refresh implementation with:
 * - Single-flight via Web Locks (or in-memory fallback for tests)
 * - keyCanSign self-check before calling /refresh
 * - PoP headers (X-PoP-*)
 * - cnf-mismatch detection (compares session_key_hash in new token vs local SPKI)
 * - 90-day family max-age / token_revoked → key-loss path
 * - Proactive scheduling with jitter
 */

import { asHttpUrl } from "@/config/endpoints";
import { buildPoP } from "./pop";
import { keyCanSign } from "./keypair";
import { parseAccessToken } from "./token";
import { bytesEqual } from "@/keys/encoding";
import { CNF_MISMATCH, type NackCode } from "./errors";

export const REFRESH_LOCK_NAME = "spawnery-refresh";

// Jitter constants for proactive refresh scheduling (AM3).
const REFRESH_MARGIN_MS = 2 * 60 * 1000;   // refresh 2 min before expiry
const REFRESH_JITTER_MS = 15 * 1000;        // ± 15 s jitter

// ── Abstractions for testability ──────────────────────────────────────────────

export interface RefreshDeps {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** SHA-256 of DER SPKI of the current session key (32 bytes). */
  localSpkiHash: Uint8Array;
  /** Current refresh-token hash (from AS on login/last refresh). */
  refreshTokenHash: Uint8Array;
  /** Injectable lock mechanism. null means "no lock support" → run without lock. */
  acquireLock?: <T>(name: string, fn: () => Promise<T>) => Promise<T>;
  /** Injectable fetch (defaults to global fetch). */
  fetchFn?: typeof fetch;
  /** Injectable current time (defaults to Date.now). */
  now?: () => number;
}

export type RefreshResult =
  | { kind: "ok"; accessToken: string; refreshTokenHash: string; expiresAt: bigint }
  | { kind: "cnf-mismatch" }
  | { kind: "revoked" }
  | { kind: "key-missing" }
  | { kind: "error"; message: string };

/** Single-flight state for environments without Web Locks (test fallback). */
let _inflight: Promise<RefreshResult> | null = null;

/**
 * refreshAccessToken performs a single /refresh round-trip with PoP.
 * Single-flight: concurrent callers share one in-flight request.
 */
export async function refreshAccessToken(deps: RefreshDeps): Promise<RefreshResult> {
  // Positive self-check: if the key is gone, take the key-loss path before any network.
  const canSign = await keyCanSign(deps.privateKey);
  if (!canSign) {
    return { kind: "key-missing" };
  }

  const lockFn =
    deps.acquireLock ??
    (typeof navigator !== "undefined" && navigator.locks
      ? <T>(name: string, fn: () => Promise<T>) =>
          navigator.locks.request(name, fn)
      : null);

  if (lockFn) {
    return lockFn(REFRESH_LOCK_NAME, () => _doRefresh(deps));
  }

  // Fallback in-memory single-flight (for tests / environments without Web Locks).
  if (_inflight) return _inflight;
  _inflight = _doRefresh(deps).finally(() => { _inflight = null; });
  return _inflight;
}

async function _doRefresh(deps: RefreshDeps): Promise<RefreshResult> {
  const fetchFn = deps.fetchFn ?? fetch;
  const now = deps.now ? new Date(deps.now()) : new Date();

  const popHeaders = await buildPoP(deps.privateKey, deps.refreshTokenHash, now);

  let res: Response;
  try {
    res = await fetchFn(asHttpUrl("/refresh"), {
      method: "POST",
      credentials: "include", // HttpOnly cookie rides on same-origin (or exact AS_ORIGIN)
      headers: {
        ...popHeaders,
      },
    });
  } catch (e) {
    return { kind: "error", message: String(e) };
  }

  if (res.status === 401) {
    const body = await res.text().catch(() => "");
    if (body.includes("token_revoked") || body.includes("family_revoked")) {
      return { kind: "revoked" };
    }
    return { kind: "error", message: `refresh 401: ${body}` };
  }

  if (!res.ok) {
    const body = await res.text().catch(() => "");
    return { kind: "error", message: `refresh ${res.status}: ${body}` };
  }

  let json: { access_token?: string; refresh_token_hash?: string };
  try {
    json = await res.json() as typeof json;
  } catch {
    return { kind: "error", message: "refresh: invalid JSON response" };
  }

  if (!json.access_token) {
    return { kind: "error", message: "refresh: missing access_token in response" };
  }

  // Decode the new token and verify cnf claim (session_key_hash must match local SPKI hash).
  let decoded;
  try {
    decoded = parseAccessToken(json.access_token);
  } catch (e) {
    return { kind: "error", message: `refresh: malformed token: ${e}` };
  }

  // cnf check: session_key_hash in the token must equal sha256(localSpki).
  if (decoded.sessionKeyHash.length > 0 && !bytesEqual(decoded.sessionKeyHash, deps.localSpkiHash)) {
    return { kind: "cnf-mismatch" };
  }

  return {
    kind: "ok",
    accessToken: json.access_token,
    refreshTokenHash: json.refresh_token_hash ?? "",
    expiresAt: decoded.expiresAt,
  };
}

/**
 * computeRefreshDelay returns the ms delay until the next proactive refresh:
 * (expiresAt - now - MARGIN) ± jitter, clamped to [0, maxDelay].
 *
 * @param expiresAt - Unix seconds BigInt from the token.
 * @param nowMs     - Current time in ms (injectable for tests).
 */
export function computeRefreshDelay(expiresAt: bigint, nowMs: number = Date.now()): number {
  const expiresMs = Number(expiresAt) * 1000;
  const targetMs = expiresMs - REFRESH_MARGIN_MS;
  const jitter = (Math.random() * 2 - 1) * REFRESH_JITTER_MS; // ±15s
  const delay = targetMs - nowMs + jitter;
  return Math.max(0, Math.min(delay, 24 * 60 * 60 * 1000)); // clamp 0..24h
}

/**
 * NACK code exported for callers that need to check cnf-mismatch.
 * (Re-exported from errors.ts for convenience.)
 */
export { CNF_MISMATCH };
export type { NackCode };
