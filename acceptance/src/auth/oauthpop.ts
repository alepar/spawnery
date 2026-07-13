/**
 * OAuthPoPAuth: the auth/v1 strategy for real OAuth+PoP targets (design §Auth; Spike S1 result,
 * sp-tq0t.3). Requires the target to run AS_FAKE_GITHUB (reachable + multi-user per T2,
 * internal/authsvc/githubfake — sp-tq0t.13).
 *
 * `Identity.token` (same shape as DevTokenAuth's, see auth/types.ts) carries the fake-IdP
 * `login_hint` in this mode, NOT a static bearer — OAuthPoPAuth mints and refreshes real,
 * short-lived (15 min) access tokens per identity, so a worker never shares an owner (design
 * §Isolation) while still getting a fresh, valid bearer across a long-running suite.
 *
 * - oracleToken/cliArgs (oracle + cliDriver): establish-then-proactively-refresh via
 *   oauth-session.ts, fully headless (Spike S1 approach (b)).
 * - seedWeb (webDriver): drives the SPA's REAL login button through a persistent Playwright page
 *   (no storageState round-trip — a non-extractable session CryptoKey cannot be serialized into
 *   one; the key lives in the page's own IndexedDB for the test, Spike S1 approach (a)). The AS
 *   does not forward login_hint to the IdP (internal/authsvc/oauth.go's serveAuthorize), so the
 *   fake-IdP hop is rewritten in-flight via page.route(), mirroring oauth-session.ts's oracle-side
 *   rewrite (sp-tq0t.13 bead notes).
 */

import type { KeyStore, ResolvedTargetVerifier } from "@spawnery/client";
import { authv1 } from "@spawnery/client";
import { fromBinary } from "@bufbuild/protobuf";
import { join } from "node:path";
import type { Identity } from "../fixtures/identity-pool";
import type { AuthStrategy, CliPreparation, CliPreparationOptions } from "./types";
import { establishOAuthSession, refreshOAuthSession, type OAuthSessionState } from "./oauth-session";
import { keyPairKeyStore } from "./keystore";
import { initializeCliOwnerDevice, runCliDeviceLogin } from "./cli-device";

// Proactive refresh margin: refresh once within this long of expiry rather than waiting for a 401.
// Mirrors web/src/auth/refresh.ts's REFRESH_MARGIN_MS and cmd/spawnctl/authstate.go's refreshWindow.
const REFRESH_MARGIN_MS = 2 * 60 * 1000;

function needsRefresh(state: OAuthSessionState, nowMs: number): boolean {
  return nowMs >= state.expiresAt - REFRESH_MARGIN_MS;
}

// --- Minimal structural Playwright surface (kept decoupled from @playwright/test at the type
// level, matching devtoken.ts's PageLike convention, so oauthpop.test.ts can exercise seedWeb
// with plain mocks). A real Playwright Page/Route/Locator satisfies this shape unchanged.
export interface RouteLike {
  request(): { url(): string };
  continue(overrides?: { url?: string }): Promise<void>;
}

export interface LocatorLike {
  click(): Promise<void>;
}

export interface PageLike {
  route(url: string | RegExp, handler: (route: RouteLike) => Promise<void> | void): Promise<void>;
  goto(url: string): Promise<unknown>;
  getByTestId(testId: string): LocatorLike;
  waitForURL(matcher: (url: URL) => boolean, options?: { timeout?: number }): Promise<void>;
}

export const OAUTH_AUTHORIZE_ROUTE = /\/oauth\/authorize(?:\?.*)?$/;

export interface OAuthPoPConfig {
  /** Base URL of the Auth Service (TargetConfig.asOrigin). */
  asOrigin: string;
  /** The SPA's origin (TargetConfig.webOrigin); the registered OAuth redirect route is
   * `${webOrigin}/callback`, matching both the AS's default derivation (cmd/authsvc/config.go's
   * derive()) and the SPA's own default (web/src/views/LoginView.tsx's REDIRECT_URI). */
  webOrigin: string;
  verifyTarget?: ResolvedTargetVerifier;
}

export class OAuthPoPAuth implements AuthStrategy {
  private readonly sessions = new Map<string, Promise<OAuthSessionState>>();
  private readonly cliPreparations = new Map<string, Promise<CliPreparation>>();

  constructor(private readonly cfg: OAuthPoPConfig) {}

  private redirectUri(): string {
    return `${this.cfg.webOrigin}/callback`;
  }

  /**
   * sessionFor returns a live session for identity, establishing it on first use and
   * proactively refreshing it once it is within REFRESH_MARGIN_MS of expiry. Each call is
   * chained onto the identity's own promise (not awaited-then-branched) so concurrent callers
   * serialize onto one flow instead of racing two establishes/refreshes against the same AS
   * refresh family (a second concurrent /refresh for an already-rotated cookie would fail).
   */
  private sessionFor(identity: Identity): Promise<OAuthSessionState> {
    const loginHint = identity.token;
    const prior =
      this.sessions.get(loginHint) ??
      establishOAuthSession({ asOrigin: this.cfg.asOrigin, redirectUri: this.redirectUri(), loginHint });
    const next = prior.then((s) =>
      needsRefresh(s, Date.now()) ? refreshOAuthSession({ asOrigin: this.cfg.asOrigin }, s) : s,
    );
    this.sessions.set(loginHint, next);
    return next;
  }

  async seedWeb(page: unknown, identity: Identity): Promise<void> {
    const p = page as PageLike;

    // Match both the SPA -> AS /oauth/authorize request and the redirected fake-IdP
    // /login/oauth/authorize hop. The AS ignores login_hint; the fake IdP consumes it.
    await p.route(OAUTH_AUTHORIZE_ROUTE, async (route) => {
      const url = new URL(route.request().url());
      if (identity.token) url.searchParams.set("login_hint", identity.token);
      await route.continue({ url: url.toString() });
    });

    await p.goto("/");
    const reachedCallback = p.waitForURL((url) =>
      url.pathname === "/callback" && url.searchParams.has("cp_access_token"), { timeout: 30_000 });
    await Promise.all([reachedCallback, p.getByTestId("sign-in-btn").click()]);

    // Wait for the SPA to consume the callback credentials. This second phase cannot resolve on
    // the pre-login "/" page, which was the race in the original single-predicate wait.
    await p.waitForURL((url) => !url.searchParams.has("cp_access_token") && url.pathname !== "/callback", {
      timeout: 30_000,
    });
  }

  async cpAccessToken(identity: Identity): Promise<string> {
    const session = await this.sessionFor(identity);
    return session.accessToken;
  }

  async nodeAccessToken(identity: Identity): Promise<string> {
    const session = await this.sessionFor(identity);
    return session.nodeAccessToken;
  }

  async accountId(identity: Identity): Promise<string> {
    const wire = await this.cpAccessToken(identity);
    const envelope = fromBinary(authv1.SignedAuthArtifactSchema, Buffer.from(wire, "base64url"));
    return fromBinary(authv1.SessionTokenBodySchema, envelope.payload).accountId;
  }

  prepareCli(page: unknown, identity: Identity, options: CliPreparationOptions): Promise<CliPreparation> {
    const key = `${identity.token}\0${options.configHome}`;
    const existing = this.cliPreparations.get(key);
    if (existing) return existing;
    // Normal spawnctl commands use os.UserConfigDir()/spawnctl. Keep the explicit key/login
    // commands in that same directory while exposing its parent as XDG_CONFIG_HOME to drivers.
    const custodyDir = join(options.configHome, "spawnctl");
    const preparation = initializeCliOwnerDevice({
      spawnctlBin: options.spawnctlBin,
      configHome: custodyDir,
    }).then(() => runCliDeviceLogin({
        spawnctlBin: options.spawnctlBin,
        asOrigin: options.asOrigin,
        configHome: custodyDir,
        page: page as Parameters<typeof runCliDeviceLogin>[0]["page"],
      }))
      .then(() => ({ authArgs: [], configHome: options.configHome }));
    this.cliPreparations.set(key, preparation);
    return preparation;
  }

  targetVerifier(_identity: Identity): ResolvedTargetVerifier {
    if (!this.cfg.verifyTarget) {
      return async () => { throw new Error("oauth-pop target verifier is not configured"); };
    }
    return this.cfg.verifyTarget;
  }

  /** Returns the session's own cnf-bound key (the same keypair that signs PoP refreshes) — the
   * CP binds the intent to the session pubkey posted at authorize time (oauth-session.ts's
   * session_pubkey), so signing with any other key would fail verification. */
  async sessionKeyStore(identity: Identity): Promise<KeyStore> {
    const session = await this.sessionFor(identity);
    return keyPairKeyStore({ privateKey: session.privateKey, publicKey: session.publicKey });
  }
}
