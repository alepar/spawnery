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

import { DEV_TOKEN_OVERRIDE_KEY } from "@spawnery/client";
import type { KeyStore, ResolvedTargetVerifier } from "@spawnery/client";
import type { Identity } from "../fixtures/identity-pool";
import type { AuthStrategy, CliPreparation, CliPreparationOptions } from "./types";
import { NodeMemoryKeyStore } from "./keystore";

export { DEV_TOKEN_OVERRIDE_KEY };

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

  async cpAccessToken(identity: Identity): Promise<string> {
    return identity.token;
  }

  async nodeAccessToken(identity: Identity): Promise<string> {
    return identity.token;
  }

  async accountId(identity: Identity): Promise<string> {
    return identity.owner;
  }

  async prepareCli(_page: unknown, identity: Identity, _options: CliPreparationOptions): Promise<CliPreparation> {
    return { authArgs: ["-token", identity.token] };
  }

  targetVerifier(_identity: Identity): ResolvedTargetVerifier {
    return async () => {};
  }

  /** A fresh key per identity — the dev CP mints the node token from the intent's own SPKI, so no
   * separate key registration is needed (unlike OAuth-PoP's cnf-bound session key). */
  async sessionKeyStore(_identity: Identity): Promise<KeyStore> {
    return new NodeMemoryKeyStore();
  }
}
