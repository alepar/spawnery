/**
 * LoginView: first-run login wall + error rendering + recovery CTAs.
 *
 * Shown when session status is:
 * - "login-required": sign in with GitHub
 * - "cnf-mismatch": session key changed on server → re-login
 * - "key-lost": session key missing (ITP eviction) → clear key + re-login
 * - "loading": spinner
 *
 * Uses buildAuthorizeUrl to construct the AS redirect with session pubkey.
 * State is always a base64url random — no routes cross this component.
 */

import { useState } from "react";
import { useSessionStore } from "@/auth/session";
import { buildAuthorizeUrl } from "@/auth/oauth";
import { getOrCreateSessionKey, exportSpkiDer } from "@/auth/keypair";
import { asErrorCopy, type AsErrorCode } from "@/auth/errors";

// The SPA's registered redirect URI. In prod this is baked via VITE_REDIRECT_URI;
// in dev it's the current origin + /callback (proxied through vite).
const REDIRECT_URI: string =
  import.meta.env.VITE_REDIRECT_URI ?? window.location.origin + "/callback";

interface LoginViewProps {
  /** AS structured error code to display (e.g. from ?error= callback param). */
  errorCode?: AsErrorCode;
}

export function LoginView({ errorCode }: LoginViewProps) {
  const { status, keyStore, logout } = useSessionStore();
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState<string | null>(null);

  async function handleSignIn() {
    setBusy(true);
    setLocalError(null);
    try {
      const kp = await getOrCreateSessionKey(keyStore);
      const spki = await exportSpkiDer(kp.publicKey);
      const url = buildAuthorizeUrl({
        redirectUri: REDIRECT_URI,
        route: window.location.pathname,
        spkiDer: spki,
      });
      window.location.href = url;
    } catch (e) {
      setLocalError(String(e));
      setBusy(false);
    }
  }

  async function handleRecoverSignIn() {
    setBusy(true);
    setLocalError(null);
    try {
      // Clear key + best-effort family revocation, then re-login.
      await logout();
      // logout() sets status=login-required; trigger sign-in immediately.
      const kp = await getOrCreateSessionKey(keyStore);
      const spki = await exportSpkiDer(kp.publicKey);
      const url = buildAuthorizeUrl({
        redirectUri: REDIRECT_URI,
        route: window.location.pathname,
        spkiDer: spki,
      });
      window.location.href = url;
    } catch (e) {
      setLocalError(String(e));
      setBusy(false);
    }
  }

  if (status === "loading") {
    return (
      <div className="flex items-center justify-center h-screen" data-testid="login-loading">
        <div className="text-muted-foreground text-sm">Loading…</div>
      </div>
    );
  }

  if (status === "cnf-mismatch") {
    return (
      <div className="flex flex-col items-center justify-center h-screen gap-4" data-testid="login-cnf-mismatch">
        <h1 className="text-lg font-semibold">Session Key Changed</h1>
        <p className="text-sm text-muted-foreground max-w-sm text-center">
          Your session key no longer matches your account. Please sign in again to re-establish your session.
        </p>
        {localError && <p className="text-destructive text-sm">{localError}</p>}
        <button
          onClick={handleRecoverSignIn}
          disabled={busy}
          className="bg-primary text-primary-foreground px-4 py-2 rounded text-sm disabled:opacity-50"
          data-testid="sign-in-recover-btn"
        >
          {busy ? "Redirecting…" : "Sign in again"}
        </button>
      </div>
    );
  }

  if (status === "key-lost") {
    return (
      <div className="flex flex-col items-center justify-center h-screen gap-4" data-testid="login-key-lost">
        <h1 className="text-lg font-semibold">Session Key Lost</h1>
        <p className="text-sm text-muted-foreground max-w-sm text-center">
          Your session key was not found (possibly cleared by the browser). Please sign in again.
        </p>
        {localError && <p className="text-destructive text-sm">{localError}</p>}
        <button
          onClick={handleRecoverSignIn}
          disabled={busy}
          className="bg-primary text-primary-foreground px-4 py-2 rounded text-sm disabled:opacity-50"
          data-testid="sign-in-recover-btn"
        >
          {busy ? "Redirecting…" : "Sign in again"}
        </button>
      </div>
    );
  }

  return (
    <div className="flex flex-col items-center justify-center h-screen gap-4" data-testid="login-view">
      <h1 className="text-xl font-bold">Spawnery</h1>
      <p className="text-sm text-muted-foreground">Sign in to continue</p>
      {errorCode && (
        <div className="bg-destructive/10 border border-destructive/30 text-destructive text-sm px-4 py-2 rounded max-w-sm text-center" data-testid="login-error">
          {asErrorCopy(errorCode)}
        </div>
      )}
      {localError && (
        <div className="bg-destructive/10 border border-destructive/30 text-destructive text-sm px-4 py-2 rounded max-w-sm text-center">
          {localError}
        </div>
      )}
      <button
        onClick={handleSignIn}
        disabled={busy}
        className="bg-primary text-primary-foreground px-6 py-2 rounded text-sm font-medium disabled:opacity-50"
        data-testid="sign-in-btn"
      >
        {busy ? "Redirecting…" : "Sign in with GitHub"}
      </button>
    </div>
  );
}
