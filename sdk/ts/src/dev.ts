/**
 * Dev-mode constants shared by every consumer that speaks to a CP running in dev-token auth mode
 * (design §Auth). Kept as a one-liner so web and acceptance stop hand-duplicating the string.
 */

/** localStorage override key the SPA's session store (web/src/auth/session.ts) reads before
 * falling back to the build-time DEV_TOKEN — lets each Playwright worker seed its own dev token
 * without a distinct build. */
export const DEV_TOKEN_OVERRIDE_KEY = "spawnery-dev-token";
