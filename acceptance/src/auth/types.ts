/**
 * AuthStrategy: pluggable per-target auth, selected by TargetConfig.authMode. DevTokenAuth seeds
 * the SPA's own token store rather than injecting an Authorization header — the SPA sources its
 * bearer from its own store (design §Auth). OAuthPoPAuth (sp-tq0t.3, auth/oauthpop.ts) implements
 * the same interface for real OAuth+PoP targets.
 *
 * cliArgs/oracleToken are Promise-returning: DevTokenAuth's are trivially synchronous under the
 * hood, but OAuthPoPAuth must drive a network round-trip (and, on a long-running suite, a PoP
 * refresh) to produce a live bearer — an async contract is required for both to share one
 * interface. For OAuthPoPAuth, `identity.token` carries the fake-IdP `login_hint` rather than a
 * static bearer (same Identity shape, mode-dependent meaning — see oauthpop.ts).
 */

import type { Identity } from "../fixtures/identity-pool";

export interface AuthStrategy {
  /** seedWeb primes a Playwright page/context with credentials BEFORE the SPA boots. */
  seedWeb(page: unknown, identity: Identity): Promise<void>;
  /** cliArgs returns the spawnctl argv fragment carrying this identity's credential. */
  cliArgs(identity: Identity): Promise<string[]>;
  /** oracleToken returns the bearer the apiDriver sends as `Authorization: Bearer <token>`. */
  oracleToken(identity: Identity): Promise<string>;
}
