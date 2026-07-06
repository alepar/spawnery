/**
 * Intent signing — thin wrapper over @spawnery/client's signing engine [AC1][AM11].
 *
 * Key invariants:
 * - Clients MUST pend an op locally before signing (AM1: "never sign unpended bytes").
 * - pollAndSign validates the CP-returned tuple against the locally-pended record.
 * - Any field mismatch between the CP tuple and the locally-pended record → refuse to sign.
 *
 * The SDK's pollAndSign needs a generated Connect `client` (unlike the old unaryFn-injected
 * version); this wrapper injects the shared web spawnServiceClient while preserving the
 * call-site signature (`{ spawnId, pended, privateKey, publicKey, maxAttempts? }`) used by
 * fork.ts/migration.ts/spawnlet.ts.
 */

import {
  domainForOp,
  buildIntentBodyBytes,
  buildSignedIntent,
  buildSessionOpenSignedIntentB64,
  registerPendedOp,
  clearPendedOp,
  pollAndSign as sdkPollAndSign,
  type IntentFields,
  type PendedOp,
  DOMAIN_CREATE_SPAWN,
  DOMAIN_RESUME_SPAWN,
  DOMAIN_RECREATE_SPAWN,
  DOMAIN_MIGRATE_SPAWN,
  DOMAIN_FORK_SPAWN,
  DOMAIN_SESSION_OPEN,
} from "@spawnery/client";
import { spawnServiceClient } from "@/api/spawnClient";

export {
  domainForOp,
  buildIntentBodyBytes,
  buildSignedIntent,
  buildSessionOpenSignedIntentB64,
  registerPendedOp,
  clearPendedOp,
  type IntentFields,
  type PendedOp,
  DOMAIN_CREATE_SPAWN,
  DOMAIN_RESUME_SPAWN,
  DOMAIN_RECREATE_SPAWN,
  DOMAIN_MIGRATE_SPAWN,
  DOMAIN_FORK_SPAWN,
  DOMAIN_SESSION_OPEN,
};

export interface PollAndSignDeps {
  spawnId: string;
  pended: PendedOp;
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** Max poll attempts before giving up */
  maxAttempts?: number;
}

/**
 * pollAndSign polls GetPendingIntent until ready, validates the tuple against
 * the locally-pended record (AM1), then builds and submits a SignedIntent.
 *
 * Returns the generated jti on success, throws on validation failure.
 */
export function pollAndSign(deps: PollAndSignDeps): Promise<string> {
  return sdkPollAndSign({ client: spawnServiceClient, ...deps });
}
