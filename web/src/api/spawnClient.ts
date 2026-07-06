/**
 * spawnServiceClient — the single browser-side generated cp.v1.SpawnService client, wiring the
 * @spawnery/client SDK transport (bearer-inject + one silent refresh-on-Unauthenticated+retry)
 * into the existing zustand session/refresh machinery.
 *
 * Only the intent RPCs (GetPendingIntent/SubmitIntent, inside auth/intent.ts's pollAndSign) use
 * this client today — all other RPCs stay on api/connect.ts's unary() (Connect-JSON over fetch),
 * unchanged.
 */

import { createClient } from "@connectrpc/connect";
import { createTransport, cpv1, type AuthProvider } from "@spawnery/client";
import { getAccessToken } from "@/auth/session";
import { CP_ORIGIN } from "@/config/endpoints";
import { tryRefresh } from "./connect";

const auth: AuthProvider = {
  getBearer: async () => getAccessToken(),
  refresh: async () => {
    await tryRefresh();
  },
};

// baseUrl mirrors cpHttpUrl semantics: the pinned CP origin in prod, same-origin (vite-proxied)
// in dev — window.location.origin as the relative-base fallback since createConnectTransport
// needs an absolute base (unlike the plain-fetch unary() path, which can use a bare relative path).
export const spawnServiceClient = createClient(
  cpv1.SpawnService,
  createTransport({ baseUrl: CP_ORIGIN || window.location.origin, auth }),
);
