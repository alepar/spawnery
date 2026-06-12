/**
 * Revocation orchestrator ([WM2][WM7][WM15]).
 *
 * Revoking a device:
 *   1. Fresh-head verify before re-seal ([WM6]).
 *   2. Append a remove entry per target via CAS retry-rebase ([WM1]).
 *   3. Init + execute the re-seal sweep to the surviving member set ([WM2]).
 *   4. Bump the pinned head version.
 *   5. If the current device is among the revoked: clear device keys + anchor.
 */

import {
  buildRemoveEntry,
  parseEntryBody,
  verifyDeviceSet,
  type ASTransport,
  type DeviceRef,
  type DeviceSetLog,
  type OwnerRoot,
} from "./deviceset";
import { type DeviceKeys, clearDeviceKeys } from "./device";
import { buildAndAppendWithRetry } from "./recovery";
import { initSweep, type SweepProgress } from "./epoch";
import { executeSweep, type SecretsCPClient } from "./sweep";
import { bumpPinnedHead, clearAnchor } from "./anchor";
import { fromBase64 } from "./encoding";
import type { DeviceListItem } from "./devicelist";

// ── Guard logic ───────────────────────────────────────────────────────────────

/**
 * isRevocableByNormalRevoke returns false for the recovery virtual device.
 * Recovery devices can only be retired via the recovery flow.
 */
export function isRevocableByNormalRevoke(item: DeviceListItem): boolean {
  return !item.isRecovery;
}

/**
 * requiresRecoveryConfirmation returns true when the revocation targets the
 * current device or would remove the last remaining non-recovery member ([WM15]).
 *
 * In those cases the UI must require the user to enter the recovery phrase
 * before proceeding (to confirm they still have access to recovery credentials).
 */
export function requiresRecoveryConfirmation(
  list: DeviceListItem[],
  targetSignPubs: string[],
): boolean {
  const targetSet = new Set(targetSignPubs);

  // Revoking the current device always requires recovery confirmation.
  const currentDevice = list.find((d) => d.isCurrent);
  if (currentDevice && targetSet.has(currentDevice.signPub)) {
    return true;
  }

  // Count non-recovery members that would survive after the revocation.
  const nonRecoveryMembers = list.filter((d) => !d.isRecovery);
  const survivors = nonRecoveryMembers.filter((d) => !targetSet.has(d.signPub));
  if (survivors.length === 0) {
    return true;
  }

  return false;
}

// ── Revocation orchestrator ───────────────────────────────────────────────────

export interface RevokeOpts {
  transport: ASTransport;
  signerKeys: DeviceKeys;
  signerRef: DeviceRef;
  ownerRoot: OwnerRoot;
  pinnedHeadVersion: number;
  /** x25519 public keys (base64) of the devices to revoke. */
  targetX25519Pubs: string[];
  /** x25519 public keys (base64) of surviving members (post-revocation). */
  survivorX25519Pubs: string[];
  /** Secret IDs for re-seal sweep ([WM2]). Empty list is valid (CP GAP sp-7h6.1). */
  secretIds: string[];
  /** CP client for the re-seal sweep. Pass a stub until sp-7h6.1 lands. */
  cpClient: SecretsCPClient;
  /** Optional progress callback during the re-seal sweep. */
  onProgress?: (progress: SweepProgress) => void;
}

/**
 * revokeDevices performs the full revocation sequence:
 *   1. Fresh-head verify.
 *   2. Append remove entries (one per target, with CAS retry-rebase).
 *   3. Init + execute re-seal sweep to survivor set.
 *   4. Bump pinned head.
 *   5. If self-revocation: clear device keys + anchor.
 *
 * Throws if any target is the recovery device (recovery targets must be
 * retired via recoverAndRotate, never via normal revoke).
 */
export async function revokeDevices(opts: RevokeOpts): Promise<SweepProgress> {
  const targetSet = new Set(opts.targetX25519Pubs);

  // Fetch + verify fresh chain before starting ([WM6])
  const { log } = await opts.transport.fetchLog();
  const verified = await verifyDeviceSet(log, opts.ownerRoot, opts.pinnedHeadVersion);

  // Reject recovery targets (recovery virtual device cannot be removed via normal revoke).
  // The recovery sign_pub changes after recoverAndRotate, so checking solely against
  // ownerRoot.recovery_sign_pub (the genesis key) fails to protect post-rotation
  // recovery devices.  Build a union of all recovery sign_pubs from the log so
  // the backstop is effective regardless of rotation history.
  const recoverySignPubs = buildRecoverySignPubs(log, opts.ownerRoot);
  for (const member of verified.members) {
    if (targetSet.has(member.x25519_pub) && recoverySignPubs.has(member.sign_pub)) {
      throw new Error(
        "revoke: the recovery virtual device cannot be removed via normal revoke; " +
        "use the recovery flow to rotate the recovery code",
      );
    }
  }

  // The signer's remove entry must be appended last: once the signer's own
  // remove entry is on the chain, subsequent verifyDeviceSet calls over the
  // updated log would reject the next iteration's entry as "not signed by a
  // current member", corrupting the chain mid-sweep (self-revoke with cascade).
  const signerX25519Pub = opts.signerRef.x25519_pub;
  const orderedTargets = targetSet.has(signerX25519Pub)
    ? [...opts.targetX25519Pubs.filter((p) => p !== signerX25519Pub), signerX25519Pub]
    : opts.targetX25519Pubs;

  // Append a remove entry for each target (CAS retry-rebase per target, [WM1])
  let currentLog = log;
  for (const targetX25519Pub of orderedTargets) {
    await buildAndAppendWithRetry(
      opts.transport,
      () => buildRemoveEntry(currentLog, targetX25519Pub, opts.signerRef, opts.signerKeys.ecdsaPrivate),
      async () => {
        const refetched = await opts.transport.fetchLog();
        await verifyDeviceSet(refetched.log, opts.ownerRoot, opts.pinnedHeadVersion);
        currentLog = refetched.log;
      },
    );
    // Re-fetch after each removal so the next removal builds on the updated chain
    const refetched = await opts.transport.fetchLog();
    await verifyDeviceSet(refetched.log, opts.ownerRoot, opts.pinnedHeadVersion);
    currentLog = refetched.log;
  }

  const finalVerified = await verifyDeviceSet(currentLog, opts.ownerRoot, opts.pinnedHeadVersion);
  const finalHeadVersion = finalVerified.headVersion;

  // Build survivor pubkey bytes for the re-seal sweep
  const survivorPubBytes = buildSurvivorPubBytes(currentLog, opts.survivorX25519Pubs);

  // Init + execute re-seal sweep ([WM2])
  const progress = initSweep({
    targetVersion: finalHeadVersion,
    secretIds: opts.secretIds,
    isRevocation: true,
    revokedX25519Pub: opts.targetX25519Pubs[0], // record first revoked device
  });

  const finalProgress = await executeSweep({
    progress,
    deviceKeys: opts.signerKeys,
    newMemberPubs: survivorPubBytes,
    cpClient: opts.cpClient,
    onProgress: opts.onProgress,
  });

  // Bump pinned head ([WM6])
  bumpPinnedHead(finalHeadVersion);

  // Self-revocation: clear local keys and anchor.
  // signerX25519Pub is already computed above from opts.signerRef.x25519_pub.
  if (targetSet.has(signerX25519Pub)) {
    await clearDeviceKeys();
    clearAnchor();
  }

  return finalProgress;
}

// ── Internal helpers ──────────────────────────────────────────────────────────

/**
 * buildSurvivorPubBytes converts survivor x25519 base64 strings to raw Uint8Array.
 * Used by executeSweep as the new recipient set.
 */
function buildSurvivorPubBytes(
  _log: DeviceSetLog,
  survivorX25519Pubs: string[],
): Uint8Array[] {
  return survivorX25519Pubs.map((pub) => fromBase64(pub));
}

/**
 * buildRecoverySignPubs returns the union of all sign_pubs that have ever been
 * the recovery virtual device:
 *   - ownerRoot.recovery_sign_pub (genesis recovery key, always included)
 *   - any "add" entry whose body.label.name === "recovery" (post-rotation devices,
 *     added by recovery.ts which always names the virtual device "recovery")
 *
 * Using only ownerRoot.recovery_sign_pub is insufficient after recoverAndRotate
 * because the live recovery device's sign_pub differs from the genesis key.
 */
function buildRecoverySignPubs(log: DeviceSetLog, ownerRoot: OwnerRoot): Set<string> {
  const pubs = new Set<string>([ownerRoot.recovery_sign_pub]);
  for (const entry of log.entries) {
    const body = parseEntryBody(entry);
    if (body.type === "add" && body.change && body.label?.name === "recovery") {
      pubs.add(body.change.sign_pub);
    }
  }
  return pubs;
}
