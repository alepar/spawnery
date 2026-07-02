/**
 * AuthStrategy: pluggable per-target auth, selected by TargetConfig.authMode. DevTokenAuth (this
 * phase) seeds the SPA's own token store rather than injecting an Authorization header — the SPA
 * sources its bearer from its own store (design §Auth). OAuthPoPAuth (sp-tq0t.3) implements the
 * same interface for real OAuth+PoP targets.
 */

import type { Identity } from "../fixtures/identity-pool";

export interface AuthStrategy {
  /** seedWeb primes a Playwright page/context with credentials BEFORE the SPA boots. */
  seedWeb(page: unknown, identity: Identity): Promise<void>;
  /** cliArgs returns the spawnctl argv fragment carrying this identity's credential. */
  cliArgs(identity: Identity): string[];
  /** oracleToken returns the bearer the apiDriver sends as `Authorization: Bearer <token>`. */
  oracleToken(identity: Identity): string;
}
