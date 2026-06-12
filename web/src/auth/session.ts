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

// Access the dev token through the env var (same source as connect.ts).
export const DEV_TOKEN: string = import.meta.env.VITE_AUTH_TOKEN ?? "";

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

  // Actions
  setToken(token: string, refreshTokenHash: string): void;
  setStatus(status: AuthStatus): void;
  getAccessToken(): string;
  bootstrap(overrideKeyStore?: KeyStore): Promise<void>;
  logout(): Promise<void>;
}

/**
 * authEnabled returns true when auth is configured or we are in production.
 * Dev mode with no AS_ORIGIN → auth disabled (dev-token fallback).
 */
export function authEnabled(): boolean {
  return !!AS_ORIGIN || import.meta.env.PROD === true;
}

export const useSessionStore = create<SessionState>((set, get) => ({
  status: "loading",
  accessToken: "",
  refreshTokenHash: "",
  account: null,
  keyStore: new IDBKeyStore(),

  setToken(token: string, rth: string) {
    let account: AccountInfo | null = null;
    try {
      const decoded = parseAccessToken(token);
      account = { accountId: decoded.accountId, handle: decoded.handle };
    } catch {
      // Ignore parse errors; account remains null.
    }
    set({ accessToken: token, refreshTokenHash: rth, account, status: "authed" });
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
      // Persist the new token.
      get().setToken(cb.accessToken, cb.refreshTokenHash);
      return;
    }
    if (cb.kind === "error") {
      // If state_mismatch or AS error, force re-login.
      set({ status: "login-required" });
      return;
    }

    // No callback — try silent refresh.
    try {
      const kp = await getOrCreateSessionKey(store);
      const spki = await exportSpkiDer(kp.publicKey);
      const spkiHash = await sessionKeyHash(spki);
      const rth = get().refreshTokenHash;

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
