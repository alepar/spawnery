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
  /** Auth Service base URL (OAuthPoPAuth's authorize/refresh target). Defaults to webOrigin,
   * mirroring the SPA's own default same-origin-via-dev-proxy behavior (web/src/config/endpoints.ts's
   * AS_ORIGIN — empty means "proxied through the web origin"). */
  asOrigin: string;
  env: string;
  targetHost: string;
  authMode: AuthMode;
  identityPool: Identity[];
  nonprodHosts: string[];
  targetRef: string;
  buildRef: string;
  spawnctlBin: string;
  /** nodeAddr is the node's terminal/exec endpoint (spawnctl exec -addr / `spawnctl exec`/attach/shell
   * dial this directly, not the CP). Only Phase-5 injection observation and @agent `exec` scenarios
   * need it; co-located/tunneled dev default assumes the CP and node run together. */
  nodeAddr: string;
  /** seedSkillAppId is a registered app whose agent installs skills (claude or codex), needed only
   * by tests/customization/injection.spec.ts to observe profile-attached skill materialization.
   * No default — unlike nodeAddr, there's no safe assumption for "which app"; the scenario fails
   * loud (never skips) when this is unset. */
  seedSkillAppId?: string;
  tokenBudget: number;
  wallclockMs: number;
  staleTtlMs: number;
  runId?: string;
  /** Pinned model for @agent scenarios (sp-tq0t.5+). Absent = @agent scenarios precondition-throw. */
  agentModel?: string;
  /** App id of a real coding-agent app on the target, for @agent scenarios. Same absence rule as agentModel. */
  agentAppId?: string;
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
    asOrigin: env.ACC_AS_ORIGIN || webOrigin,
    env: env.ACC_ENV ?? "dev",
    targetHost,
    authMode,
    identityPool: parseIdentityPool(env.ACC_IDENTITY_POOL!),
    nonprodHosts: (env.ACC_NONPROD_HOSTS ?? "").split(",").map((s) => s.trim()).filter((s) => s.length > 0),
    targetRef: env.ACC_TARGET_REF!,
    buildRef: env.ACC_BUILD_REF!,
    spawnctlBin: env.ACC_SPAWNCTL_BIN ?? "spawnctl",
    nodeAddr: env.ACC_NODE_ADDR ?? "http://127.0.0.1:9092",
    seedSkillAppId: env.ACC_SEED_SKILL_APP_ID || undefined,
    tokenBudget: Number(env.ACC_TOKEN_BUDGET ?? "200000"),
    wallclockMs: Number(env.ACC_WALLCLOCK_MS ?? "1800000"),
    staleTtlMs: Number(env.ACC_STALE_TTL_MS ?? "3600000"),
    runId: env.ACC_RUN_ID || undefined,
    agentModel: env.ACC_AGENT_MODEL || undefined,
    agentAppId: env.ACC_AGENT_APP_ID || undefined,
  };
}
