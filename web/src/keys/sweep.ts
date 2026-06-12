/**
 * Re-seal sweep executor ([WM2]).
 *
 * After any device-set mutation (add or remove), every existing secret must be
 * re-sealed to the new member set so that:
 *   - a newly enrolled device can open all secrets (add case), and
 *   - a removed device is locked out of future secrets (revocation case).
 *
 * The executor fetches each envelope from the CP, opens it, re-seals to the
 * new set with a fresh DEK [roast M2], and stores the result back. Progress
 * survives a browser restart via the SweepProgress in localStorage (epoch.ts).
 *
 * CP RPC GAP: the concrete Connect/protobuf client for GetSecret / PutSecret
 * is deferred until the sp-7h6.1 user-secrets-store slice lands. Callers
 * provide a SecretsCPClient implementation; tests pass a fake. Once the CP
 * RPCs ship, wire the real client here.
 */

import { type Envelope, reSealEnvelope } from "./hpke";
import { type DeviceKeys, exportDeviceRef } from "./device";
import {
  markSecretsCompleted,
  markSecretFailed,
  type SweepProgress,
} from "./epoch";
import { toBase64 } from "./encoding";

/**
 * SecretsCPClient is the seam between the sweep executor and the CP's secrets
 * store. Implementations call the CP's GetSecret / PutSecret RPCs.
 *
 * CP RPC GAP: the real Connect client is deferred (sp-7h6.1). Until it ships,
 * callers must supply a fake/stub that satisfies this interface — no real
 * secrets will be re-sealed in the browser until the real client is wired.
 */
export interface SecretsCPClient {
  /**
   * getEnvelope fetches the current sealed envelope for the given secret.
   * The returned Envelope is the at-rest JSON representation matching Go's
   * seal.Envelope (field names snake_case, bytes base64-encoded).
   */
  getEnvelope(secretId: string): Promise<Envelope>;

  /**
   * putEnvelope stores a re-sealed envelope, replacing the existing version.
   * The at_rest.version in env must be exactly (old version + 1); the CP
   * enforces monotonic versioning as a CAS guard.
   */
  putEnvelope(secretId: string, env: Envelope): Promise<void>;
}

/**
 * executeSweep drives the actual re-seal of all pending secrets in a sweep.
 *
 * For each unprocessed secret ID:
 *   1. fetch the current sealed envelope from the CP
 *   2. open it with this device's private key
 *   3. re-seal the plaintext to the new member set with a fresh DEK [roast M2]
 *   4. store the re-sealed envelope back
 *   5. mark the secret as completed (or failed) and persist progress
 *
 * Resumable: pass the stored SweepProgress (from loadSweepProgress) and
 * executeSweep will skip already-completed secrets. The returned progress
 * reflects the final state.
 *
 * CP RPC GAP: cpClient must be a real implementation once the CP GetSecret /
 * PutSecret Connect RPCs land (sp-7h6.1). Until then only stubs are usable.
 */
export async function executeSweep(opts: {
  progress: SweepProgress;
  deviceKeys: DeviceKeys;
  /** X25519 pubkeys (raw 32 bytes) of every member in the NEW member set. */
  newMemberPubs: Uint8Array[];
  cpClient: SecretsCPClient;
  onProgress?: (progress: SweepProgress) => void;
}): Promise<SweepProgress> {
  let progress = opts.progress;

  // Export this device's public key bytes (needed for recipient-side DH in open)
  const ref = await exportDeviceRef(opts.deviceKeys);
  const devicePubBytes = ref.x25519Pub;

  const completedSet = new Set(progress.completed);
  const secretIds = progress.secretIds;

  for (const secretId of secretIds) {
    if (completedSet.has(secretId)) continue; // already done in a prior run

    try {
      // 1. Fetch the current sealed envelope
      const env = await opts.cpClient.getEnvelope(secretId);

      // 2 + 3. Open and re-seal to the new member set (fresh DEK [roast M2])
      const resealed = await reSealEnvelope(
        env,
        opts.deviceKeys.x25519Private,
        devicePubBytes,
        opts.newMemberPubs,
      );

      // 4. Store the re-sealed envelope
      await opts.cpClient.putEnvelope(secretId, resealed);

      // 5. Mark complete and persist
      progress = markSecretsCompleted(progress, [secretId]);
      completedSet.add(secretId);
      opts.onProgress?.(progress);
    } catch {
      // Record failure; the secret will be retried on the next executeSweep call
      progress = markSecretFailed(progress, secretId);
      opts.onProgress?.(progress);
    }
  }

  return progress;
}

// Re-export toBase64 so callers that only import sweep.ts can encode pubkeys.
// (Avoids a double import of encoding.ts in UI components.)
export { toBase64 };
