/**
 * OAuth authorize + callback handling for the SPA.
 *
 * buildAuthorizeUrl: generates state, stores {state, route} in sessionStorage,
 *   builds the AS /oauth/authorize redirect URL.
 * parseCallback: reads ?access_token=&state= (or ?error=&error_description=) from
 *   the current location.search, verifies state, strips the URL from history.
 */

import { asHttpUrl } from "@/config/endpoints";
import { toBase64Url } from "./token";

const STORAGE_KEY = "spawnery-oauth-state";

interface StoredState {
  state: string;
  route: string;
}

// ── sessionStorage abstraction (injectable for tests) ────────────────────────

export interface StateStorage {
  get(key: string): string | null;
  set(key: string, value: string): void;
  remove(key: string): void;
}

export const sessionStateStorage: StateStorage = {
  get: (k) => sessionStorage.getItem(k),
  set: (k, v) => sessionStorage.setItem(k, v),
  remove: (k) => sessionStorage.removeItem(k),
};

// ── URL history abstraction (injectable for tests) ────────────────────────────

export interface HistoryFacade {
  replaceState(url: string): void;
  /** Current location.search string (e.g. "?access_token=..."). */
  locationSearch(): string;
  /** Current location.pathname. */
  locationPathname(): string;
}

export const browserHistory: HistoryFacade = {
  replaceState: (url) => window.history.replaceState(null, "", url),
  locationSearch: () => window.location.search,
  locationPathname: () => window.location.pathname,
};

// ── buildAuthorizeUrl ─────────────────────────────────────────────────────────

export interface AuthorizeOptions {
  /** The registered SPA redirect URI (absolute, e.g. https://app.example.com/oauth/callback). */
  redirectUri: string;
  /** The current SPA route to restore after login (e.g. "/spawns/abc"). */
  route: string;
  /** DER SPKI of the session public key (required by AS, [R2/AM5]). */
  spkiDer: Uint8Array;
  storage?: StateStorage;
}

/**
 * buildAuthorizeUrl builds the AS /oauth/authorize URL, persisting state+route
 * in sessionStorage for CSRF/fixation protection (AM8).
 *
 * Returns the full URL to redirect the browser to.
 */
export function buildAuthorizeUrl({
  redirectUri,
  route,
  spkiDer,
  storage = sessionStateStorage,
}: AuthorizeOptions): string {
  // Generate random state (16 random bytes as base64url).
  const stateBytes = crypto.getRandomValues(new Uint8Array(16));
  const state = toBase64Url(stateBytes);

  // Persist state + route for callback verification.
  const stored: StoredState = { state, route };
  storage.set(STORAGE_KEY, JSON.stringify(stored));

  // Encode the session pubkey as base64 (standard, not URL-safe — matches AS parseSessionSPKI).
  const spkiB64 = btoa(String.fromCharCode(...spkiDer));

  // Build URL; handle both absolute (AS_ORIGIN configured) and relative (dev proxy) paths.
  const base = asHttpUrl("/oauth/authorize");
  const params = new URLSearchParams({
    redirect_uri: redirectUri,
    state,
    session_pubkey: spkiB64,
  });
  return `${base}?${params.toString()}`;
}

// ── parseCallback ─────────────────────────────────────────────────────────────

export type CallbackResult =
  | { kind: "ok"; accessToken: string; refreshTokenHash: string; route: string }
  | { kind: "error"; code: string; description: string }
  | { kind: "none" }; // no callback params present

/**
 * parseCallback reads callback parameters from the URL search string.
 *
 * On success:
 * - Verifies the returned state matches the stored one (AM8 CSRF/fixation).
 * - Strips the token from the URL via history.replaceState.
 * - Returns {kind:"ok", accessToken, refreshTokenHash, route}.
 *
 * On error: returns {kind:"error", code, description}.
 * If no callback params: returns {kind:"none"}.
 */
export function parseCallback(
  storage: StateStorage = sessionStateStorage,
  history: HistoryFacade = browserHistory,
): CallbackResult {
  const search = history.locationSearch();
  if (!search) return { kind: "none" };

  const params = new URLSearchParams(search);
  const accessToken = params.get("access_token");
  const stateParam = params.get("state");
  const errorCode = params.get("error");

  // No callback parameters at all.
  if (!accessToken && !errorCode) return { kind: "none" };

  // Error callback.
  if (errorCode) {
    storage.remove(STORAGE_KEY);
    const description = params.get("error_description") ?? "";
    // Strip error params from URL to avoid user confusion on reload.
    history.replaceState(history.locationPathname());
    return { kind: "error", code: errorCode, description };
  }

  // Success callback — verify state (AM8).
  const storedRaw = storage.get(STORAGE_KEY);
  storage.remove(STORAGE_KEY);

  if (!storedRaw) {
    history.replaceState(history.locationPathname());
    return { kind: "error", code: "state_mismatch", description: "No stored OAuth state" };
  }

  let stored: StoredState;
  try {
    stored = JSON.parse(storedRaw) as StoredState;
  } catch {
    history.replaceState(history.locationPathname());
    return { kind: "error", code: "state_mismatch", description: "Corrupt OAuth state" };
  }

  if (stateParam !== stored.state) {
    history.replaceState(history.locationPathname());
    return { kind: "error", code: "state_mismatch", description: "State mismatch (CSRF check)" };
  }

  const refreshTokenHash = params.get("refresh_token_hash") ?? "";

  // Strip token + state from URL (access_token must not survive in browser history).
  history.replaceState(history.locationPathname());

  return {
    kind: "ok",
    accessToken: accessToken!,
    refreshTokenHash,
    route: stored.route,
  };
}
