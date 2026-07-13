/**
 * oauth-session: drives the SPA's real OAuth+PoP wire flow headlessly against an AS_FAKE_GITHUB
 * target (Spike S1 result, approach (b) — see sp-tq0t.1's bead notes and the design's §Auth /
 * Spikes section). This is the network layer OAuthPoPAuth (oauthpop.ts) builds on; it holds no
 * per-identity state itself — callers thread OAuthSessionState through establish -> refresh.
 *
 * Flow (mirrors internal/authsvc/oauth_test.go's triggerCallbackWith helper — the Go test rig for
 * the exact same wire sequence — and cmd/spawnctl/authstate.go's PoP reproduction):
 *
 *   1. GET  {asOrigin}/oauth/authorize?redirect_uri&state&session_pubkey  (redirect: manual)
 *        -> 302 Location = the fake IdP's authorize URL; Set-Cookie: as_flow=<flow> [AM8].
 *   2. Rewrite the fake IdP URL to add login_hint=<loginHint> — the AS does NOT forward login_hint
 *      to the IdP (internal/authsvc/oauth.go's serveAuthorize), so per-identity user selection
 *      happens by rewriting this hop directly (sp-tq0t.13 bead notes; T2's login_hint contract).
 *   3. GET  <rewritten fake IdP authorize URL>  (redirect: manual)
 *        -> 302 Location = {asOrigin}/oauth/callback?code&state (AS's own GitHubRedirectURI).
 *   4. GET  <callback URL> with Cookie: as_flow=<flow>  (redirect: manual)
 *        -> 302 Location = {redirectUri}?cp_access_token&node_access_token&state&refresh_token_hash
 *           (non-loopback path,
 *           AM5/R1); Set-Cookie: refresh_token=<raw>, HttpOnly, Path=/refresh.
 *   5. (refreshOAuthSession) POST {asOrigin}/refresh with Cookie: refresh_token=<raw> + PoP headers
 *        -> {cp_access_token, node_access_token, refresh_token_hash} JSON;
 *           Set-Cookie: refresh_token=<rotated>.
 *
 * The oracle never navigates a browser to redirectUri — it parses the token straight out of the
 * Location header, exactly as a same-process oracle can when it (unlike a browser) is also able to
 * read the Set-Cookie response headers directly.
 */

import { buildPoP, fromBase64Url } from "@spawnery/client";

/** ACCESS_TOKEN_TTL_MS mirrors the AS's fixed 15-minute access-token TTL (Spike S1 result). Not
 * parsed from the token body — cmd/spawnctl/authstate.go's accessTokenTTLClient does the same. */
export const ACCESS_TOKEN_TTL_MS = 15 * 60 * 1000;

export interface OAuthSessionState {
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** CP-audience access credential (legacy field name retained for existing browser callers). */
  accessToken: string;
  /** Node-audience half of the AS-issued credential pair. */
  nodeAccessToken: string;
  refreshTokenRaw: string;
  refreshTokenHash: Uint8Array;
  /** Unix ms this session's access token is expected to expire (mint time + ACCESS_TOKEN_TTL_MS). */
  expiresAt: number;
}

export interface EstablishOAuthSessionConfig {
  /** Base URL of the Auth Service (e.g. TargetConfig.asOrigin). */
  asOrigin: string;
  /** The SPA's registered OAuth redirect route (e.g. `${webOrigin}/callback`). */
  redirectUri: string;
  /** Selects (or deterministically auto-registers) a distinct fake-GitHub owner; empty = the
   * fake's default user. See internal/authsvc/githubfake's login_hint contract (T2/sp-tq0t.13). */
  loginHint: string;
}

class OAuthSessionError extends Error {
  constructor(message: string) {
    super(`oauth-session: ${message}`);
    this.name = "OAuthSessionError";
  }
}

async function generateSessionKey(): Promise<CryptoKeyPair> {
  return (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
}

/** findCookie extracts one cookie's value from a fetch Response's Set-Cookie headers. */
function findCookie(res: Response, name: string): string | undefined {
  for (const raw of res.headers.getSetCookie()) {
    const pair = raw.split(";", 1)[0];
    const eq = pair.indexOf("=");
    if (eq < 0) continue;
    if (pair.slice(0, eq).trim() === name) return pair.slice(eq + 1).trim();
  }
  return undefined;
}

function requireHeader(res: Response, name: string, context: string): string {
  const v = res.headers.get(name);
  if (!v) throw new OAuthSessionError(`${context}: response had no ${name} header (status ${res.status})`);
  return v;
}

function requireCookie(res: Response, name: string, context: string): string {
  const v = findCookie(res, name);
  if (!v) throw new OAuthSessionError(`${context}: response set no ${name} cookie (status ${res.status})`);
  return v;
}

async function require302(res: Response, context: string): Promise<void> {
  if (res.status !== 302) {
    // Include the body (a JSON {error,...} on the AS's own endpoints) — a non-302 here is most
    // often an operator-fixable target-config problem (e.g. redirect_uri not registered), so the
    // diagnostic is worth the extra read.
    const body = await res.text().catch(() => "");
    throw new OAuthSessionError(`${context}: expected 302, got ${res.status}: ${body}`);
  }
}

/** establishOAuthSession drives one full authorize -> IdP -> callback round-trip (steps 1-4 above). */
export async function establishOAuthSession(cfg: EstablishOAuthSessionConfig): Promise<OAuthSessionState> {
  const { privateKey, publicKey } = await generateSessionKey();
  const spkiDer = new Uint8Array(await crypto.subtle.exportKey("spki", publicKey));
  const spkiB64 = Buffer.from(spkiDer).toString("base64");
  const clientState = Buffer.from(crypto.getRandomValues(new Uint8Array(16))).toString("base64url");

  // 1. GET /oauth/authorize.
  const authorizeUrl = `${cfg.asOrigin}/oauth/authorize?${new URLSearchParams({
    redirect_uri: cfg.redirectUri,
    state: clientState,
    session_pubkey: spkiB64,
  }).toString()}`;
  const authResp = await fetch(authorizeUrl, { redirect: "manual" });
  await require302(authResp, "/oauth/authorize");
  const idpLocation = requireHeader(authResp, "location", "/oauth/authorize");
  const flowCookie = requireCookie(authResp, "as_flow", "/oauth/authorize");

  // 2. Rewrite the fake IdP's authorize URL to select this identity's owner.
  const idpUrl = new URL(idpLocation);
  if (cfg.loginHint) idpUrl.searchParams.set("login_hint", cfg.loginHint);

  // 3. GET the fake IdP authorize URL (auto-approves; no interaction, see githubfake.go).
  const idpResp = await fetch(idpUrl.toString(), { redirect: "manual" });
  await require302(idpResp, "fake IdP authorize");
  const callbackUrl = requireHeader(idpResp, "location", "fake IdP authorize");

  // 4. GET /oauth/callback, carrying the flow cookie [AM8].
  const cbResp = await fetch(callbackUrl, {
    redirect: "manual",
    headers: { Cookie: `as_flow=${flowCookie}` },
  });
  await require302(cbResp, "/oauth/callback");
  const finalLocation = requireHeader(cbResp, "location", "/oauth/callback");
  const finalParams = new URL(finalLocation).searchParams;

  const errorCode = finalParams.get("error");
  if (errorCode) {
    throw new OAuthSessionError(
      `/oauth/callback redirected with error=${errorCode}: ${finalParams.get("error_description") ?? ""}`,
    );
  }
  const returnedState = finalParams.get("state");
  if (returnedState !== clientState) {
    throw new OAuthSessionError(`/oauth/callback state mismatch: sent ${clientState}, got ${returnedState}`);
  }
  const accessToken = finalParams.get("cp_access_token");
  const nodeAccessToken = finalParams.get("node_access_token");
  const refreshTokenHashB64 = finalParams.get("refresh_token_hash");
  if (!accessToken) throw new OAuthSessionError("/oauth/callback redirect had no cp_access_token");
  if (!nodeAccessToken) throw new OAuthSessionError("/oauth/callback redirect had no node_access_token");
  if (!refreshTokenHashB64) {
    // A bare-IP loopback redirectUri (http://127.0.0.1:.../callback or [::1]) makes the AS treat
    // this as a NATIVE-APP client (RFC 8252 §7.3 — see internal/authsvc/oauth.go's isLoopbackURI)
    // and deliver refresh_token directly in the query instead of a hash + cookie. Name it
    // explicitly: this is the most likely operator mistake to produce this error (e.g. pointing
    // ACC_WEB_ORIGIN at a raw 127.0.0.1 URL for local testing — use "localhost" instead).
    const hint = finalParams.has("refresh_token")
      ? " (redirectUri resolved to a loopback host, which the AS treats as a native-app client and delivers refresh_token directly instead of refresh_token_hash — use a non-loopback host, e.g. localhost instead of 127.0.0.1, for ACC_WEB_ORIGIN)"
      : "";
    throw new OAuthSessionError(`/oauth/callback redirect had no refresh_token_hash${hint}`);
  }
  const refreshTokenRaw = requireCookie(cbResp, "refresh_token", "/oauth/callback");

  return {
    privateKey,
    publicKey,
    accessToken,
    nodeAccessToken,
    refreshTokenRaw,
    refreshTokenHash: fromBase64Url(refreshTokenHashB64),
    expiresAt: Date.now() + ACCESS_TOKEN_TTL_MS,
  };
}

export interface RefreshOAuthSessionConfig {
  asOrigin: string;
}

/** refreshOAuthSession performs one PoP-signed POST /refresh round-trip, rotating the token. */
export async function refreshOAuthSession(
  cfg: RefreshOAuthSessionConfig,
  prev: OAuthSessionState,
): Promise<OAuthSessionState> {
  const popHeaders = await buildPoP(prev.privateKey, prev.refreshTokenHash);
  const res = await fetch(`${cfg.asOrigin}/refresh`, {
    method: "POST",
    headers: { ...popHeaders, Cookie: `refresh_token=${prev.refreshTokenRaw}` },
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new OAuthSessionError(`/refresh failed: ${res.status} ${body}`);
  }
  const body = (await res.json()) as {
    cp_access_token?: string;
    node_access_token?: string;
    refresh_token_hash?: string;
  };
  if (!body.cp_access_token) throw new OAuthSessionError("/refresh response had no cp_access_token");
  if (!body.node_access_token) throw new OAuthSessionError("/refresh response had no node_access_token");
  if (!body.refresh_token_hash) throw new OAuthSessionError("/refresh response had no refresh_token_hash");
  const refreshTokenRaw = requireCookie(res, "refresh_token", "/refresh");

  return {
    ...prev,
    accessToken: body.cp_access_token,
    nodeAccessToken: body.node_access_token,
    refreshTokenRaw,
    refreshTokenHash: fromBase64Url(body.refresh_token_hash),
    expiresAt: Date.now() + ACCESS_TOKEN_TTL_MS,
  };
}
