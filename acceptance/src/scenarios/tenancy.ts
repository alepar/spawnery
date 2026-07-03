/**
 * Phase 6 (tenancy) support: env-var config loading for the non-leakage/quota specs, a
 * Connect-JSON error classifier, and a raw CreateSpawn caller used to observe the N+1 rejection
 * (ApiDriver.createSpawn throws on !ok and discards the body — the quota spec needs the parsed
 * error, so it bypasses ApiDriver here rather than widening its error type for one caller).
 *
 * Per the design's tenancy scenario: A sees A, not B; quota (ResourceExhausted at N+1) runs ONLY
 * on a target with a known non-zero per-owner cap (internal/cp/server.go's checkSpawnQuota
 * treats maxSpawnsPerOwner<=0 as unlimited, so a black-box suite can neither set nor reliably
 * discover it) — see the phase README for the env vars this loader reads.
 */

import { parseIdentityPool, type Identity } from "../fixtures/identity-pool";
import type { CreateSpawnApiRequest } from "../drivers/api";

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

/** classifyConnectError pulls code/message out of a parsed Connect-JSON error body (defensive against a non-object body). */
export function classifyConnectError(_status: number, body: unknown): { code?: string; message?: string } {
  if (typeof body !== "object" || body === null) return {};
  const b = body as Record<string, unknown>;
  const code = typeof b.code === "string" ? b.code : undefined;
  const message = typeof b.message === "string" ? b.message : undefined;
  return { ...(code !== undefined ? { code } : {}), ...(message !== undefined ? { message } : {}) };
}

/** isResourceExhausted reports whether a rawCreateSpawn result is the N+1 ResourceExhausted rejection. */
export function isResourceExhausted(r: { status: number; code?: string }): boolean {
  return r.status === 429 && r.code === "resource_exhausted";
}

/**
 * rawCreateSpawn POSTs CreateSpawn directly (mirroring ApiDriver.call's headers) and returns the
 * parsed status/code/message instead of throwing on !ok — the quota spec needs to observe and
 * assert on the N+1 rejection, not just know it happened.
 */
export async function rawCreateSpawn(
  cpEndpoint: string,
  token: string,
  req: CreateSpawnApiRequest,
): Promise<{ status: number; code?: string; message?: string }> {
  const res = await fetch(`${cpEndpoint}/cp.v1.SpawnService/CreateSpawn`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify(req),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    return { status: res.status, ...classifyConnectError(res.status, body) };
  }
  return { status: res.status };
}
