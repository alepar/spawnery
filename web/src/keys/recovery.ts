/**
 * Recovery flow state machine (Phase 6, [WM12]).
 *
 * Web recovery flow: a user whose only device is lost recovers in-product.
 *
 * Flow (spec §2 "Recovery flow"):
 *   1. Show the M8 trusted-device warning verbatim ([WM12]).
 *   2. User enters the BIP-39 recovery phrase.
 *   3. Derive recovery device keys in-page.
 *   4. Verify the chain with the (re-derived) recovery signing pubkey as root.
 *   5. Enroll a fresh device — sign an add-entry for the fresh browser device.
 *   6. Re-seal existing secrets (sweep, [WM2]).
 *   7. Force recovery-code rotation:
 *      a. Generate a new recovery phrase (new virtual device).
 *      b. Append an add-entry for the new recovery device.
 *      c. Append a remove-entry for the old recovery device.
 *      d. Persist new phrase to display to user.
 *   8. The old recovery key is retired — the user must record the new phrase.
 *
 * This is the one flow where seed material exists in page memory. The copy
 * says so explicitly ([WM12]). Phrase inputs have autocomplete="off".
 */

import { deriveDeviceKeysFromMnemonic, generateDeviceKeys, exportDeviceRef, storeDeviceKeys } from "./device";
import { generateMnemonic } from "./bip39";
import {
  buildAddEntry,
  buildRemoveEntry,
  appendEntry,
  fetchDeviceSetLog,
  verifyDeviceSet,
  ConflictError,
  type DeviceRef,
  type OwnerRoot,
  type StoredEntry,
} from "./deviceset";
import { initSweep, type SweepProgress } from "./epoch";
import { toBase64 } from "./encoding";

/**
 * M8 trusted-device warning — verbatim from the owner-sealed spec §3, displayed
 * before any recovery-code entry. Never skip this ([WM12]).
 */
export const M8_TRUSTED_DEVICE_WARNING = `Recovery requires entering your BIP-39 phrase. \
This phrase is the master key for all your sealed secrets. \
Only enter it on a device you personally control and trust: the machine running \
this browser must not be shared, observed, or compromised. \
In a hotel or on someone else's computer, cancel and use an enrolled device instead.`;

export interface RecoveryResult {
  /** The new recovery phrase to display and have the user record. */
  newRecoveryPhrase: string;
  /** Sweep progress for re-sealing to the new device set ([WM2]). */
  sweepProgress: SweepProgress;
  /** Whether navigator.storage.persist() was granted for the new device keys. */
  persistGranted: boolean;
}

/**
 * recoverAndRotate performs the full in-browser recovery flow:
 *   1. Derive the recovery device from the entered phrase.
 *   2. Verify the chain — confirm the recovery signing key is actually enrolled.
 *   3. Enroll a fresh new device (sign add-entry with recovery key).
 *   4. Rotate the recovery code (add new, remove old recovery device).
 *   5. Persist the fresh device keys in IndexedDB.
 *   6. Return the new recovery phrase and sweep progress.
 *
 * @param recoveryPhrase - The BIP-39 phrase entered by the user.
 * @param newDeviceName - Name for the fresh device being enrolled.
 * @param ownerRoot - The pinned OwnerRoot (must be locally persisted, not from AS).
 * @param pinnedHeadVersion - The pinned head version ([WM6]).
 * @param asUrl - AS base URL.
 * @param bearerToken - Bearer token for AS API calls.
 * @param secretIds - List of secret IDs for the re-seal sweep ([WM2]).
 */
export async function recoverAndRotate(opts: {
  recoveryPhrase: string;
  newDeviceName: string;
  ownerRoot: OwnerRoot;
  pinnedHeadVersion: number;
  asUrl: string;
  bearerToken: string;
  secretIds: string[];
}): Promise<RecoveryResult> {
  // Derive recovery keys from the entered phrase
  const recoveryKeys = await deriveDeviceKeysFromMnemonic(opts.recoveryPhrase);
  const recoveryRef = await exportDeviceRef(recoveryKeys);
  const recoveryDSRef: DeviceRef = {
    x25519_pub: toBase64(recoveryRef.x25519Pub),
    sign_pub: toBase64(recoveryRef.signPub),
  };

  // Fetch and verify the chain ([WM6] head-regression check)
  const { log } = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
  const verified = await verifyDeviceSet(log, opts.ownerRoot, opts.pinnedHeadVersion);

  // Confirm the recovery key is actually a current member
  const isMember = verified.members.some((m) => m.sign_pub === recoveryDSRef.sign_pub);
  if (!isMember) {
    throw new Error(
      "recovery: the entered phrase does not correspond to an enrolled recovery device — " +
      "check the phrase and try again",
    );
  }

  // Generate fresh device keys for this browser
  const freshDeviceKeys = await generateDeviceKeys();
  const freshRef = await exportDeviceRef(freshDeviceKeys);
  const freshDSRef: DeviceRef = {
    x25519_pub: toBase64(freshRef.x25519Pub),
    sign_pub: toBase64(freshRef.signPub),
  };

  // Generate the new recovery phrase
  const newRecoveryPhrase = await generateMnemonic();
  const newRecoveryKeys = await deriveDeviceKeysFromMnemonic(newRecoveryPhrase);
  const newRecoveryRef = await exportDeviceRef(newRecoveryKeys);
  const newRecoveryDSRef: DeviceRef = {
    x25519_pub: toBase64(newRecoveryRef.x25519Pub),
    sign_pub: toBase64(newRecoveryRef.signPub),
  };

  // Step 3: Enroll the fresh device (signed by recovery key)
  let currentLog = log;
  await buildAndAppendWithRetry(
    () => buildAddEntry(currentLog, freshDSRef, opts.newDeviceName, recoveryDSRef, recoveryKeys.ecdsaPrivate),
    async () => {
      const refetched = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
      await verifyDeviceSet(refetched.log, opts.ownerRoot);
      currentLog = refetched.log;
    },
    opts.asUrl,
    opts.bearerToken,
  );

  // Rebuild currentLog to include the fresh device entry
  const { log: logAfterAdd } = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
  currentLog = logAfterAdd;

  // Step 4a: Enroll the new recovery virtual device (signed by recovery key)
  await buildAndAppendWithRetry(
    () => buildAddEntry(currentLog, newRecoveryDSRef, "recovery", recoveryDSRef, recoveryKeys.ecdsaPrivate),
    async () => {
      const refetched = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
      await verifyDeviceSet(refetched.log, opts.ownerRoot);
      currentLog = refetched.log;
    },
    opts.asUrl,
    opts.bearerToken,
  );

  // Rebuild again
  const { log: logAfterAddRecovery } = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
  currentLog = logAfterAddRecovery;

  // Step 4b: Remove the OLD recovery virtual device (signed by fresh device — it's now enrolled)
  await buildAndAppendWithRetry(
    () => buildRemoveEntry(currentLog, recoveryDSRef.x25519_pub, freshDSRef, freshDeviceKeys.ecdsaPrivate),
    async () => {
      const refetched = await fetchDeviceSetLog(opts.asUrl, opts.bearerToken);
      await verifyDeviceSet(refetched.log, opts.ownerRoot);
      currentLog = refetched.log;
    },
    opts.asUrl,
    opts.bearerToken,
  );

  // Persist the fresh device keys ([WM11])
  const { persistGranted } = await storeDeviceKeys(freshDeviceKeys);

  // Init re-seal sweep ([WM2])
  const sweepProgress = initSweep({
    targetVersion: verified.headVersion + 3, // 3 mutations: addFresh, addNewRecovery, removeOldRecovery
    secretIds: opts.secretIds,
    isRevocation: true,
    revokedX25519Pub: recoveryDSRef.x25519_pub,
  });

  return { newRecoveryPhrase, sweepProgress, persistGranted };
}

// ── Helpers ───────────────────────────────────────────────────────────────────

async function buildAndAppendWithRetry(
  buildFn: () => Promise<StoredEntry>,
  rebaseFn: () => Promise<void>,
  asUrl: string,
  bearerToken: string,
  maxRetries = 5,
): Promise<{ version: number; head: string }> {
  let retries = 0;
  while (true) {
    const entry = await buildFn();
    try {
      return await appendEntry(asUrl, bearerToken, entry);
    } catch (e) {
      if (e instanceof ConflictError && retries < maxRetries) {
        retries++;
        await rebaseFn();
        continue;
      }
      throw e;
    }
  }
}
