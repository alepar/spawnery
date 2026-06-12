/**
 * Typed error codes for AS errors (OAuth callback) and node NACK codes.
 *
 * These mirror Go constants in:
 *   internal/authsvc/oauth.go (redirectError codes)
 *   internal/node/intentverify.go (NACK codes)
 */

// ── AS OAuth error codes (from callback ?error= param) ───────────────────────

export type AsErrorCode =
  | "registration_closed"
  | "access_denied"
  | "invalid_request"
  | "server_error"
  | "state_mismatch"   // SPA-side CSRF/fixation check (not from AS)
  | "unknown";

// ── Node NACK codes (from Connect error or WS close reason) ──────────────────

export type NackCode =
  | "CNF_MISMATCH"      // session key hash does not match the token's cnf claim
  | "CORRESPONDENCE"    // intent fields don't match the CP's committed tuple
  | "STALE"             // intent is too old
  | "SKEW"              // issued_at too far in the future
  | "REPLAY"            // jti already consumed
  | "UNKNOWN_NACK";

export const CNF_MISMATCH: NackCode = "CNF_MISMATCH";

// ── Error mapping ─────────────────────────────────────────────────────────────

/**
 * mapAsError maps a raw AS callback ?error= string to the typed union.
 */
export function mapAsError(raw: string | null | undefined): AsErrorCode {
  switch (raw) {
    case "registration_closed": return "registration_closed";
    case "access_denied":       return "access_denied";
    case "invalid_request":     return "invalid_request";
    case "server_error":        return "server_error";
    case "state_mismatch":      return "state_mismatch";
    default:                    return "unknown";
  }
}

/**
 * mapNackCode extracts a NackCode from a Connect error message or WS close reason.
 * Searches for known code strings in the error text.
 */
export function mapNackCode(text: string): NackCode {
  if (text.includes("CNF_MISMATCH"))   return "CNF_MISMATCH";
  if (text.includes("CORRESPONDENCE")) return "CORRESPONDENCE";
  if (text.includes("STALE"))          return "STALE";
  if (text.includes("SKEW"))           return "SKEW";
  if (text.includes("REPLAY"))         return "REPLAY";
  return "UNKNOWN_NACK";
}

export function isCnfMismatch(code: NackCode): boolean {
  return code === "CNF_MISMATCH";
}

// ── Human-readable copy (used in LoginView) ───────────────────────────────────

export function asErrorCopy(code: AsErrorCode): string {
  switch (code) {
    case "registration_closed": return "Registrations are currently closed. Please contact the administrator.";
    case "access_denied":       return "Access was denied. Please try signing in again.";
    case "invalid_request":     return "Login request was invalid. Please try again.";
    case "server_error":        return "A server error occurred. Please try again later.";
    case "state_mismatch":      return "Security check failed. Please try signing in again.";
    default:                    return "An unexpected error occurred. Please try signing in again.";
  }
}

export function nackCopy(code: NackCode): string {
  switch (code) {
    case "CNF_MISMATCH":    return "Your session key no longer matches your token. Please sign in again.";
    case "CORRESPONDENCE":  return "Intent validation failed. Please retry the operation.";
    case "STALE":           return "Your request expired. Please retry.";
    case "SKEW":            return "Your device clock may be out of sync. Please retry.";
    case "REPLAY":          return "Duplicate request detected. Please retry.";
    default:                return "Operation rejected by the server. Please retry.";
  }
}
