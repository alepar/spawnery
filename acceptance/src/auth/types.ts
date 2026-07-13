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
 *
 * sessionKeyStore supplies the AcceptanceClient (drivers/oracle.ts) with the KeyStore its
 * SpawnClient signs A4 intents with: DevTokenAuth hands back a fresh-key NodeMemoryKeyStore (the
 * dev CP mints the node token from the intent's own SPKI, so no separate key registration is
 * needed); OAuth-PoP MUST return the session's own cnf-bound key (the same key that signs PoP
 * refreshes) — signing with a different key would mismatch the CP's cnf binding.
 */

import type { KeyStore, ResolvedTargetVerifier } from "@spawnery/client";
import type { Identity } from "../fixtures/identity-pool";

export interface CliPreparationOptions {
  spawnctlBin: string;
  asOrigin: string;
  configHome: string;
}

export interface CliPreparation {
  /** Empty for stored OAuth state; dev-token mode carries its explicit test-only bearer here. */
  authArgs: string[];
  configHome?: string;
}

export interface AuthStrategy {
  /** seedWeb primes a Playwright page/context with credentials BEFORE the SPA boots. */
  seedWeb(page: unknown, identity: Identity): Promise<void>;
  /** Explicit audience-separated accessors. One refresh replaces the pair atomically. */
  cpAccessToken(identity: Identity): Promise<string>;
  nodeAccessToken(identity: Identity): Promise<string>;
  /** Prepare spawnctl custody. OAuth implementations must use login --device, never key export. */
  prepareCli(page: unknown, identity: Identity, options: CliPreparationOptions): Promise<CliPreparation>;
  /** Strict target verifier invoked before any SDK intent signature is emitted. */
  targetVerifier(identity: Identity): ResolvedTargetVerifier;
  /** sessionKeyStore returns the KeyStore AcceptanceClient should sign intents with for this
   * identity (see the header note above). */
  sessionKeyStore(identity: Identity): Promise<KeyStore>;
}
