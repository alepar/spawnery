/**
 * DevTokenAuth: the auth/v1 strategy for CP_DEV_TOKENS-style targets (design §Auth).
 *
 * seedWeb does NOT inject an Authorization header — the SPA sources its bearer from its own
 * session store (web/src/auth/session.ts's getAccessToken()), which in dev mode reads a
 * localStorage override key before falling back to the build-time DEV_TOKEN. Seeding that key via
 * page.addInitScript (run before any app script) gives each Playwright worker its own dev token
 * without needing distinct builds per worker. See the matching SPA-side seam in
 * web/src/auth/session.ts.
 */

import type { Identity } from "../fixtures/identity-pool";
import type { AuthStrategy } from "./types";

// Must match web/src/auth/session.ts's DEV_TOKEN_OVERRIDE_KEY exactly — the two packages are
// independent (acceptance/ is not a dependency of web/), so the key is duplicated, not imported.
export const DEV_TOKEN_OVERRIDE_KEY = "spawnery-dev-token";

interface PageLike {
  addInitScript(fn: (arg: [string, string]) => void, arg: [string, string]): Promise<void>;
}

export class DevTokenAuth implements AuthStrategy {
  async seedWeb(page: unknown, identity: Identity): Promise<void> {
    await (page as PageLike).addInitScript(
      ([key, value]) => {
        window.localStorage.setItem(key, value);
      },
      [DEV_TOKEN_OVERRIDE_KEY, identity.token],
    );
  }

  cliArgs(identity: Identity): string[] {
    return ["-token", identity.token];
  }

  oracleToken(identity: Identity): string {
    return identity.token;
  }
}
