/**
 * Phase 6 (tenancy) support: env-var config loading for the non-leakage/quota specs and the
 * ResourceExhausted classifier the quota spec asserts on. AcceptanceClient.createSpawn now goes
 * through @spawnery/client, which throws a structured ConnectError on a non-2xx RPC — so the
 * N+1 rejection is observed via a try/catch around AcceptanceClient.createSpawn itself, rather
 * than a raw fetch bypassing it (the old ApiDriver.createSpawn threw a plain Error and discarded
 * the parsed body, forcing a bypass; that's no longer true here).
 *
 * Per the design's tenancy scenario: A sees A, not B; quota (ResourceExhausted at N+1) runs ONLY
 * on a target with a known non-zero per-owner cap (internal/cp/server.go's checkSpawnQuota
 * treats maxSpawnsPerOwner<=0 as unlimited, so a black-box suite can neither set nor reliably
 * discover it) — see the phase README for the env vars this loader reads.
 */

import { ConnectError, Code } from "@spawnery/client";
import { parseIdentityPool, type Identity } from "../fixtures/identity-pool";

export interface TenancyConfig {
  appId: string;
  model?: string;
  a: Identity;
  b: Identity;
  quota?: { identity: Identity; owner: string; cap: number };
}

/**
 * loadTenancyConfig parses the ACC_APP_ID/ACC_MODEL/ACC_TENANCY_A/ACC_TENANCY_B/ACC_QUOTA_* env
 * vars into a TenancyConfig. Throws on a missing ACC_APP_ID, on A/B resolving to the same owner,
 * or on an ACC_IDENTITY_POOL fallback with fewer than 2 entries — all three make the tenancy
 * property under test meaningless. A missing/non-positive/non-numeric ACC_QUOTA_CAP is NOT an
 * error: it leaves `quota` undefined, config-gating the quota spec off (design: "known-cap target
 * only").
 */
export function loadTenancyConfig(env: NodeJS.ProcessEnv = process.env): TenancyConfig {
  const appId = env.ACC_APP_ID;
  if (!appId) throw new Error("missing required env var ACC_APP_ID (see acceptance/tests/tenancy/README.md)");

  const a = resolveIdentity(env, "ACC_TENANCY_A", 0, env.ACC_IDENTITY_POOL);
  const b = resolveIdentity(env, "ACC_TENANCY_B", 1, env.ACC_IDENTITY_POOL);
  if (a.owner === b.owner) {
    throw new Error(
      `ACC_TENANCY_A and ACC_TENANCY_B resolved to the same owner ${JSON.stringify(a.owner)} — ` +
        `a tenancy test needs two distinct owners`,
    );
  }

  const cap = Number(env.ACC_QUOTA_CAP);
  const quota =
    env.ACC_QUOTA_TOKEN && Number.isFinite(cap) && cap > 0
      ? { identity: { token: env.ACC_QUOTA_TOKEN, owner: env.ACC_QUOTA_OWNER ?? "" }, owner: env.ACC_QUOTA_OWNER ?? "", cap }
      : undefined;

  return { appId, model: env.ACC_MODEL, a, b, quota };
}

/** resolveIdentity reads an explicit "token=owner" var, falling back to ACC_IDENTITY_POOL[index]. */
function resolveIdentity(env: NodeJS.ProcessEnv, varName: string, poolIndex: number, poolSpec: string | undefined): Identity {
  const explicit = env[varName];
  if (explicit) return parseIdentityPool(explicit)[0];
  if (!poolSpec) {
    throw new Error(`${varName} is unset and ACC_IDENTITY_POOL is also unset — cannot resolve a fallback identity`);
  }
  const pool = parseIdentityPool(poolSpec);
  if (pool.length < 2) {
    throw new Error(
      `${varName} is unset and the ACC_IDENTITY_POOL fallback has only ${pool.length} entry(ies) — ` +
        `the tenancy test needs 2 distinct identities; set ${varName} explicitly or grow the pool`,
    );
  }
  return pool[poolIndex];
}

/** isResourceExhausted reports whether e is the N+1 ResourceExhausted rejection (HTTP 429), as
 * thrown by AcceptanceClient.createSpawn (a structured ConnectError, not a raw fetch error). */
export function isResourceExhausted(e: unknown): boolean {
  return e instanceof ConnectError && e.code === Code.ResourceExhausted;
}
