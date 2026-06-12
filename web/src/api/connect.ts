// Calls the CP's ConnectRPC unary methods via plain fetch (Connect JSON, camelCase fields).
import { cpHttpUrl } from "@/config/endpoints";
import { getAccessToken, authEnabled, DEV_TOKEN as SESSION_DEV_TOKEN, useSessionStore } from "@/auth/session";
import { refreshAccessToken } from "@/auth/refresh";
import { getOrCreateSessionKey, exportSpkiDer, sessionKeyHash } from "@/auth/keypair";

// Re-export DEV_TOKEN for backward-compat (WS consumers, spawnlet.ts re-export).
export const DEV_TOKEN = SESSION_DEV_TOKEN;

/**
 * unary performs a Connect-JSON unary RPC.
 *
 * In auth-enabled mode: injects Bearer token from the session store.
 * On 401: attempts one silent refresh + retry.
 * In dev mode (auth disabled): falls back to DEV_TOKEN.
 */
export async function unary<T>(method: string, body: unknown): Promise<T> {
  const token = getAccessToken();
  const res = await _doUnary(method, body, token);

  // On 401 in auth-enabled mode: try one silent refresh + retry.
  if (res.status === 401 && authEnabled()) {
    const refreshed = await _tryRefresh();
    if (refreshed) {
      const newToken = getAccessToken();
      const retryRes = await _doUnary(method, body, newToken);
      if (!retryRes.ok) throw new Error(`${method} failed: ${retryRes.status} ${await retryRes.text()}`);
      return (await retryRes.json()) as T;
    }
    // Refresh failed → set login-required, but only if _tryRefresh hasn't already set a
    // more specific status (cnf-mismatch or key-lost). Those drive distinct recovery UX
    // in LoginView (spec §5) and must not be clobbered by the generic catch-all.
    const s = useSessionStore.getState();
    if (s.status !== "cnf-mismatch" && s.status !== "key-lost") {
      s.setStatus("login-required");
    }
    throw new Error(`${method}: session expired, please sign in again`);
  }

  if (!res.ok) throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}

async function _doUnary(method: string, body: unknown, token: string): Promise<Response> {
  return fetch(cpHttpUrl(`/cp.v1.SpawnService/${method}`), {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify(body),
  });
}

/** Try a silent refresh; returns true on success, false on failure. */
async function _tryRefresh(): Promise<boolean> {
  try {
    const session = useSessionStore.getState();
    const store = session.keyStore;
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const spkiHash = await sessionKeyHash(spki);
    const rthB64 = session.refreshTokenHash;
    const rth = rthB64 ? _b64urlToBytes(rthB64) : new Uint8Array(32);

    const result = await refreshAccessToken({
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      localSpkiHash: spkiHash,
      refreshTokenHash: rth,
    });

    if (result.kind === "ok") {
      session.setToken(result.accessToken, result.refreshTokenHash);
      return true;
    }
    if (result.kind === "cnf-mismatch") {
      session.setStatus("cnf-mismatch");
    } else if (result.kind === "revoked" || result.kind === "key-missing") {
      session.setStatus("key-lost");
    }
    return false;
  } catch {
    return false;
  }
}

function _b64urlToBytes(s: string): Uint8Array {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
