/**
 * TargetConfig: the live spawnery instance this run points at, loaded from env vars
 * (see .env.example for the full list) plus the URL-based prod-safety guardrail.
 *
 * The guardrail derives "is this target non-prod" from the ACTUAL target host, not a
 * self-declared label — and is default-deny: a host that isn't explicitly allowlisted (or
 * localhost/127.0.0.1) is treated as prod. See design §Isolation / guardrail.
 */

import { parseIdentityPool, type Identity } from "../fixtures/identity-pool";

export type AuthMode = "dev-token" | "oauth-pop";

export interface TargetConfig {
  webOrigin: string;
  cpEndpoint: string;
  env: string;
  targetHost: string;
  authMode: AuthMode;
  identityPool: Identity[];
  nonprodHosts: string[];
  targetRef: string;
  buildRef: string;
  spawnctlBin: string;
  tokenBudget: number;
  wallclockMs: number;
  staleTtlMs: number;
  runId?: string;
}

const REQUIRED_VARS = ["ACC_WEB_ORIGIN", "ACC_CP_ENDPOINT", "ACC_IDENTITY_POOL", "ACC_TARGET_REF", "ACC_BUILD_REF"] as const;

/**
 * isNonProd is the default-deny prod-safety check: a host is non-prod only if it is
 * localhost/127.0.0.1 or an explicit member (exact or suffix match) of the allowlist. Any
 * unrecognized host is treated as prod (returns false).
 */
export function isNonProd(host: string, allowlist: string[]): boolean {
  const h = host.toLowerCase();
  if (h === "localhost" || h === "127.0.0.1") return true;
  return allowlist.some((entry) => {
    const e = entry.trim().toLowerCase();
    if (e.length === 0) return false;
    return h === e || h.endsWith("." + e);
  });
}

/** loadTargetConfig parses ACC_* env vars into a TargetConfig, throwing on missing required vars. */
export function loadTargetConfig(env: NodeJS.ProcessEnv = process.env): TargetConfig {
  for (const key of REQUIRED_VARS) {
    if (!env[key]) throw new Error(`missing required env var ${key} (see acceptance/.env.example)`);
  }
  const webOrigin = env.ACC_WEB_ORIGIN!;
  const cpEndpoint = env.ACC_CP_ENDPOINT!;
  let targetHost: string;
  try {
    targetHost = new URL(webOrigin).host;
  } catch {
    throw new Error(`ACC_WEB_ORIGIN ${JSON.stringify(webOrigin)} is not a valid URL`);
  }
  const authMode = (env.ACC_AUTH_MODE ?? "dev-token") as AuthMode;
  if (authMode !== "dev-token" && authMode !== "oauth-pop") {
    throw new Error(`ACC_AUTH_MODE must be "dev-token" or "oauth-pop", got ${JSON.stringify(authMode)}`);
  }
  return {
    webOrigin,
    cpEndpoint,
    env: env.ACC_ENV ?? "dev",
    targetHost,
    authMode,
    identityPool: parseIdentityPool(env.ACC_IDENTITY_POOL!),
    nonprodHosts: (env.ACC_NONPROD_HOSTS ?? "").split(",").map((s) => s.trim()).filter((s) => s.length > 0),
    targetRef: env.ACC_TARGET_REF!,
    buildRef: env.ACC_BUILD_REF!,
    spawnctlBin: env.ACC_SPAWNCTL_BIN ?? "spawnctl",
    tokenBudget: Number(env.ACC_TOKEN_BUDGET ?? "200000"),
    wallclockMs: Number(env.ACC_WALLCLOCK_MS ?? "1800000"),
    staleTtlMs: Number(env.ACC_STALE_TTL_MS ?? "3600000"),
    runId: env.ACC_RUN_ID || undefined,
  };
}
