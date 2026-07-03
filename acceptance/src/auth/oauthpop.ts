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

import type { Identity } from "../fixtures/identity-pool";
import type { AuthStrategy } from "./types";
import { establishOAuthSession, refreshOAuthSession, type OAuthSessionState } from "./oauth-session";

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
  route(url: string, handler: (route: RouteLike) => Promise<void> | void): Promise<void>;
  goto(url: string): Promise<unknown>;
  getByTestId(testId: string): LocatorLike;
  waitForURL(matcher: (url: URL) => boolean, options?: { timeout?: number }): Promise<void>;
}

export interface OAuthPoPConfig {
  /** Base URL of the Auth Service (TargetConfig.asOrigin). */
  asOrigin: string;
  /** The SPA's origin (TargetConfig.webOrigin); the registered OAuth redirect route is
   * `${webOrigin}/callback`, matching both the AS's default derivation (cmd/authsvc/config.go's
   * derive()) and the SPA's own default (web/src/views/LoginView.tsx's REDIRECT_URI). */
  webOrigin: string;
}

export class OAuthPoPAuth implements AuthStrategy {
  private readonly sessions = new Map<string, Promise<OAuthSessionState>>();

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

    await p.route("**/login/oauth/authorize*", async (route) => {
      const url = new URL(route.request().url());
      if (identity.token) url.searchParams.set("login_hint", identity.token);
      await route.continue({ url: url.toString() });
    });

    await p.goto("/");
    await p.getByTestId("sign-in-btn").click();

    // SOFT SPOT (not yet exercised against a live target — mirrors webDriver.ts's two flagged
    // spots): App.tsx normalizes "/" -> "/templates" once authed (client-side, no reload), so the
    // eventual URL is /templates; this only waits for the callback's transient query/path to
    // clear, which is enough for the SPA to have consumed the token and settled into "authed".
    await p.waitForURL((url) => !url.searchParams.has("access_token") && url.pathname !== "/callback", {
      timeout: 30_000,
    });
  }

  async cliArgs(identity: Identity): Promise<string[]> {
    const session = await this.sessionFor(identity);
    return ["-token", session.accessToken];
  }

  async oracleToken(identity: Identity): Promise<string> {
    const session = await this.sessionFor(identity);
    return session.accessToken;
  }
}
