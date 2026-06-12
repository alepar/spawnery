/**
 * Session store (zustand): holds the access token in memory only, account info,
 * and the auth lifecycle status.
 *
 * bootstrap() is called once at startup (main.tsx) to:
 * 1. In dev mode (auth disabled): set status=authed with DEV_TOKEN.
 * 2. Parse a callback if present in the URL.
 * 3. Attempt a silent /refresh.
 * 4. On failure: status=login-required.
 */

import { create } from "zustand";
import { AS_ORIGIN } from "@/config/endpoints";
import { parseCallback, browserHistory, sessionStateStorage } from "./oauth";
import { getOrCreateSessionKey, exportSpkiDer, sessionKeyHash, clearSessionKey } from "./keypair";
import { refreshAccessToken } from "./refresh";
import { parseAccessToken } from "./token";
import { IDBKeyStore, type KeyStore } from "./keystore";
import { mapAsError, type AsErrorCode } from "./errors";

// Access the dev token through the env var (same source as connect.ts).
export const DEV_TOKEN: string = import.meta.env.VITE_AUTH_TOKEN ?? "";

// Key used to persist refresh_token_hash across cold reloads. The hash alone does not
// allow refreshing (HttpOnly cookie + session key are also required), so localStorage
// exposure is safe [AM2/AM5].
export const RTH_STORAGE_KEY = "spawnery-rth";

function _saveRth(rth: string): void {
  try { localStorage.setItem(RTH_STORAGE_KEY, rth); } catch { /* private-browsing fallback */ }
}
function _loadRth(): string {
  try { return localStorage.getItem(RTH_STORAGE_KEY) ?? ""; } catch { return ""; }
}
function _clearRth(): void {
  try { localStorage.removeItem(RTH_STORAGE_KEY); } catch { /* ignore */ }
}

export type AuthStatus =
  | "loading"
  | "login-required"
  | "authed"
  | "cnf-mismatch"
  | "key-lost";

export interface AccountInfo {
  accountId: string;
  handle: string;
}

export interface SessionState {
  status: AuthStatus;
  accessToken: string;
  refreshTokenHash: string; // SHA-256 of current refresh token (from AS, for PoP)
  account: AccountInfo | null;
  keyStore: KeyStore;
  /** AS error code carried from the callback into the store so App can render it. */
  callbackErrorCode: AsErrorCode | null;

  // Actions
  setToken(token: string, refreshTokenHash: string): void;
  setStatus(status: AuthStatus): void;
  getAccessToken(): string;
  bootstrap(overrideKeyStore?: KeyStore): Promise<void>;
  logout(): Promise<void>;
}

/**
 * authEnabled returns true when auth is configured or we are in production.
 * Dev mode with no AS_ORIGIN and no VITE_AUTH_ENABLED → auth disabled (dev-token fallback).
 * VITE_AUTH_ENABLED=1 forces auth in dev for e2e auth testing without requiring AS_ORIGIN.
 */
export function authEnabled(): boolean {
  return !!AS_ORIGIN || !!import.meta.env.VITE_AUTH_ENABLED || import.meta.env.PROD === true;
}

export const useSessionStore = create<SessionState>((set, get) => ({
  status: "loading",
  accessToken: "",
  refreshTokenHash: "",
  account: null,
  keyStore: new IDBKeyStore(),
  callbackErrorCode: null,

  setToken(token: string, rth: string) {
    // Persist the hash before updating zustand so cold-reload bootstrap() can read it [AM2].
    _saveRth(rth);
    let account: AccountInfo | null = null;
    try {
      const decoded = parseAccessToken(token);
      account = { accountId: decoded.accountId, handle: decoded.handle };
    } catch {
      // Ignore parse errors; account remains null.
    }
    set({ accessToken: token, refreshTokenHash: rth, account, status: "authed", callbackErrorCode: null });
  },

  setStatus(status: AuthStatus) {
    set({ status });
  },

  getAccessToken() {
    const s = get();
    if (!authEnabled()) return DEV_TOKEN;
    return s.accessToken;
  },

  async bootstrap(overrideKeyStore?: KeyStore) {
    if (!authEnabled()) {
      // Dev mode: bypass auth entirely.
      set({ status: "authed", accessToken: DEV_TOKEN, refreshTokenHash: "", account: null });
      return;
    }

    const store = overrideKeyStore ?? get().keyStore;
    if (overrideKeyStore) set({ keyStore: store });

    // Check if we are on the callback URL.
    const cb = parseCallback(sessionStateStorage, browserHistory);
    if (cb.kind === "ok") {
      // Persist the new token and restore the original pre-login route.
      get().setToken(cb.accessToken, cb.refreshTokenHash);
      if (cb.route) browserHistory.replaceState(cb.route);
      return;
    }
    if (cb.kind === "error") {
      // Carry the AS error code into the store so App/LoginView can display it.
      set({ status: "login-required", callbackErrorCode: mapAsError(cb.code) });
      return;
    }

    // No callback — try silent refresh.
    try {
      const kp = await getOrCreateSessionKey(store);
      const spki = await exportSpkiDer(kp.publicKey);
      const spkiHash = await sessionKeyHash(spki);
      // On cold reload, refreshTokenHash is empty in zustand memory; read from localStorage.
      const rth = get().refreshTokenHash || _loadRth();

      const result = await refreshAccessToken({
        privateKey: kp.privateKey,
        publicKey: kp.publicKey,
        localSpkiHash: spkiHash,
        refreshTokenHash: rth.length > 0 ? _base64urlToBytes(rth) : new Uint8Array(32),
      });

      if (result.kind === "ok") {
        get().setToken(result.accessToken, result.refreshTokenHash);
        return;
      }
      if (result.kind === "cnf-mismatch") {
        set({ status: "cnf-mismatch" });
        return;
      }
      if (result.kind === "revoked" || result.kind === "key-missing") {
        // Clear the key and force fresh login.
        await clearSessionKey(store);
        set({ status: "key-lost" });
        return;
      }
    } catch {
      // Ignore network/parse errors on bootstrap → login-required.
    }

    set({ status: "login-required" });
  },

  async logout() {
    const store = get().keyStore;
    _clearRth();
    // Best-effort: revoke the server-side family.
    try {
      await fetch(/* AS_ORIGIN + */ "/logout", {
        method: "POST",
        credentials: "include",
      });
    } catch {
      // Ignore.
    }
    await clearSessionKey(store);
    set({ status: "login-required", accessToken: "", refreshTokenHash: "", account: null });
  },
}));

/** getAccessToken is the accessor for transport layers (connect.ts, WS bind frames). */
export function getAccessToken(): string {
  return useSessionStore.getState().getAccessToken();
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function _base64urlToBytes(s: string): Uint8Array {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
