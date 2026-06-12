/**
 * Enrollment state machine (Phase 5, [WM4][WM5][WM2][WM7]).
 *
 * Bidirectional enrollment (spec §2 D4): any enrolled device (web or
 * `spawnctl key approve`) can approve a new device. The flow:
 *
 * Enrollee side:
 *   1. generateEnrollmentLink() — creates keys, returns a one-time short-lived
 *      link carrying the new device's pubkeys.
 *   2. Waits for the approval response: OwnerRoot + current head.
 *   3. Pins OwnerRoot + head locally and verifies the chain against the
 *      received root ([WM5]: never TOFU the anchor from the first AS fetch).
 *   4. Stores device keys in IndexedDB.
 *
 * Approver side:
 *   1. Receives the enrollment link (QR or URL).
 *   2. Computes the SAS from (genesis hash ‖ head ‖ new pubkeys) ([WM4]).
 *   3. User confirms the SAS matches what the enrollee displays.
 *   4. approveEnrollment() — appends the add-entry, triggers re-seal sweep
 *      ([WM2]), and returns OwnerRoot + head to the enrollee ([WM5]).
 *   5. Optionally surfaces cascade devices (enrolled by the approver) ([WM7]).
 *
 * The approval response MUST NOT come from the AS — it comes from the approving
 * device directly (e.g. via the enrollment link response channel or out-of-band).
 */

import {
  buildAddEntry,
  verifyDeviceSet,
  ConflictError,
  type ASTransport,
  type DeviceRef,
  type OwnerRoot,
  type StoredEntry,
  type DeviceSetLog,
} from "./deviceset";
import {
  generateDeviceKeys,
  exportDeviceRef,
  storeDeviceKeys,
  type DeviceKeys,
} from "./device";
import { deriveSAS } from "./sas";
import { initSweep, type SweepProgress } from "./epoch";
import { saveAnchor } from "./anchor";
import { toBase64, fromBase64 } from "./encoding";

// ── Enrollment link (enrollee side) ──────────────────────────────────────────

export interface EnrollmentPayload {
  /** New device's X25519 public key (base64). */
  x25519Pub: string;
  /** New device's ECDSA-P256 public key (base64, SEC1 uncompressed). */
  signPub: string;
  /** Proposed device name (self-asserted, confirmed by approver as label). */
  deviceName: string;
  /** ISO8601 expiry — the link is single-use and short-lived. */
  expiresAt: string;
}

export interface EnrollmentLinkResult {
  keys: DeviceKeys;
  payload: EnrollmentPayload;
  /** Enrolled-at nanos, used for the label if the link is approved. */
  enrolledAtNanos: string;
}

/**
 * generateEnrollmentPayload generates fresh device keys and builds the
 * enrollment payload. The payload is what goes into the QR/link.
 *
 * Does NOT persist keys yet — only persists after approval is received.
 */
export async function generateEnrollmentPayload(
  deviceName: string,
): Promise<EnrollmentLinkResult> {
  const keys = await generateDeviceKeys();
  const ref = await exportDeviceRef(keys);
  const enrolledAtNanos = (BigInt(Date.now()) * 1_000_000n).toString();
  const expiresAt = new Date(Date.now() + 15 * 60 * 1000).toISOString(); // 15 min TTL
  const payload: EnrollmentPayload = {
    x25519Pub: toBase64(ref.x25519Pub),
    signPub: toBase64(ref.signPub),
    deviceName,
    expiresAt,
  };
  return { keys, payload, enrolledAtNanos };
}

/** Serialize enrollment payload to a URL-safe base64 string for the link/QR. */
export function serializeEnrollmentPayload(payload: EnrollmentPayload): string {
  const json = JSON.stringify(payload);
  const bytes = new TextEncoder().encode(json);
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, "");
}

/** Deserialize enrollment payload from a URL-safe base64 string. */
export function deserializeEnrollmentPayload(encoded: string): EnrollmentPayload {
  const b64 = encoded.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return JSON.parse(new TextDecoder().decode(bytes)) as EnrollmentPayload;
}

// ── SAS derivation (both sides) ───────────────────────────────────────────────

/**
 * computeEnrollmentSAS derives the SAS for this enrollment attempt.
 * Both approver and enrollee must independently derive this code and
 * the human confirms they match (code is NEVER parsed from the link, [WM4]).
 */
export async function computeEnrollmentSAS(
  genesisHash: Uint8Array,
  headHash: Uint8Array,
  payload: EnrollmentPayload,
): Promise<string> {
  return deriveSAS(
    genesisHash,
    headHash,
    fromBase64(payload.x25519Pub),
    fromBase64(payload.signPub),
  );
}

// ── Approver side ─────────────────────────────────────────────────────────────

export interface ApprovalResult {
  /** OwnerRoot to send to the enrollee. */
  ownerRoot: OwnerRoot;
  /** Chain head hash after the add entry (base64). */
  headHash: string;
  /** Chain head version after the add entry. */
  headVersion: number;
  /** Sweep progress for re-sealing to the expanded device set ([WM2]). */
  sweepProgress: SweepProgress;
  /**
   * Cascade devices: all devices enrolled by transitively through the approver
   * chain. Surface to the user if they want to remove them in the same sweep
   * ([WM7]).
   */
  cascadeDevices: DeviceRef[];
}

/**
 * approveEnrollment appends an add-entry for the new device, triggers
 * re-seal sweep tracking, and returns the data the enrollee needs.
 *
 * Retries on CAS conflict ([WM1]: retry-rebase).
 * The approver's OwnerRoot and head are returned to the enrollee ([WM5]).
 */
export async function approveEnrollment(opts: {
  payload: EnrollmentPayload;
  transport: ASTransport;
  approverKeys: DeviceKeys;
  ownerRoot: OwnerRoot;
  secretIds: string[]; // list of secret IDs for the re-seal sweep ([WM2])
  pinnedHeadVersion?: number; // WM6
}): Promise<ApprovalResult> {
  const approverRef = await exportDeviceRef(opts.approverKeys);
  const approverDSRef: DeviceRef = {
    x25519_pub: toBase64(approverRef.x25519Pub),
    sign_pub: toBase64(approverRef.signPub),
  };
  const newDeviceRef: DeviceRef = {
    x25519_pub: opts.payload.x25519Pub,
    sign_pub: opts.payload.signPub,
  };

  // Fetch and verify current chain
  const { log } = await opts.transport.fetchLog();
  await verifyDeviceSet(log, opts.ownerRoot, opts.pinnedHeadVersion);

  // Build the add entry and attempt to append with CAS retry-rebase ([WM1])
  let addEntry: StoredEntry;
  let appendResult: { version: number; head: string };
  let currentLog = log;
  let retries = 0;
  const MAX_RETRIES = 5;

  while (true) {
    addEntry = await buildAddEntry(
      currentLog,
      newDeviceRef,
      opts.payload.deviceName,
      approverDSRef,
      opts.approverKeys.ecdsaPrivate,
    );
    try {
      appendResult = await opts.transport.append(addEntry);
      break;
    } catch (e) {
      if (e instanceof ConflictError && retries < MAX_RETRIES) {
        // Rebase: re-fetch the chain and rebuild on top of the new head ([WM1])
        retries++;
        const refetched = await opts.transport.fetchLog();
        await verifyDeviceSet(refetched.log, opts.ownerRoot, opts.pinnedHeadVersion);
        currentLog = refetched.log;
        continue;
      }
      throw e;
    }
  }

  // Init re-seal sweep ([WM2])
  const sweepProgress = initSweep({
    targetVersion: appendResult.version,
    secretIds: opts.secretIds,
    isRevocation: false,
  });

  // Cascade devices: find any devices enrolled by the approver ([WM7])
  const cascadeDevices = findCascadeDevices(currentLog, approverDSRef.sign_pub);

  return {
    ownerRoot: opts.ownerRoot,
    headHash: appendResult.head,
    headVersion: appendResult.version,
    sweepProgress,
    cascadeDevices,
  };
}

// ── Enrollee side — receive approval ─────────────────────────────────────────

export interface EnrollmentApproval {
  ownerRoot: OwnerRoot;
  headHash: string;
  headVersion: number;
}

/**
 * finalizeEnrollment receives the approval response, pins the OwnerRoot + head,
 * verifies the chain from the AS, and persists the device keys ([WM5]).
 *
 * [WM5]: OwnerRoot comes from the approval, not from the AS. We NEVER TOFU
 * the anchor from the first AS fetch.
 */
export async function finalizeEnrollment(opts: {
  pendingKeys: DeviceKeys;
  approval: EnrollmentApproval;
  transport: ASTransport;
}): Promise<{ persistGranted: boolean }> {
  // Verify the chain against the received OwnerRoot ([WM5])
  const { log } = await opts.transport.fetchLog();
  await verifyDeviceSet(log, opts.approval.ownerRoot, opts.approval.headVersion);

  // Persist device keys ([WM11])
  const { persistGranted } = await storeDeviceKeys(opts.pendingKeys);

  // Pin OwnerRoot + head version locally ([WM5])
  saveAnchor({ ownerRoot: opts.approval.ownerRoot, headVersion: opts.approval.headVersion });

  return { persistGranted };
}

// ── Internal helpers ──────────────────────────────────────────────────────────

/**
 * findCascadeDevices finds devices that were enrolled by the given signer's
 * key (for the revocation cascade prompt, [WM7]).
 */
function findCascadeDevices(log: DeviceSetLog, signerSignPub: string): DeviceRef[] {
  const cascade: DeviceRef[] = [];
  for (const entry of log.entries) {
    const raw = fromBase64(entry.body);
    const body = JSON.parse(new TextDecoder().decode(raw)) as {
      type: string;
      change?: DeviceRef;
    };
    if (body.type !== "add" || !body.change) continue;
    // Check if this add entry was signed by our signer
    const signedByUs = entry.sigs.some((s) => s.signer_pub === signerSignPub);
    if (signedByUs) cascade.push(body.change);
  }
  return cascade;
}
