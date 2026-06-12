/**
 * Verified device list builder ([WM5][WM15]).
 *
 * Builds a UI-ready list of DeviceListItems from the raw device-set log,
 * extracting authenticated labels and enrollment timestamps from signed
 * entry bodies. Fail-closed: rethrows verifyDeviceSet errors — callers
 * must never render without a verified chain.
 *
 * Recovery device identification:
 *   - genesis devices[1] where sign_pub matches ownerRoot.recovery_sign_pub, OR
 *   - an "add" entry whose body.label.name === "recovery" (reserved name used by
 *     recovery.ts for the rotation virtual device).
 *
 * Constraint: the reserved name "recovery" must not be used for real devices.
 * If a real device is named "recovery" it will be misclassified as the recovery
 * virtual device. recovery.ts always names the recovery virtual device "recovery"
 * — this is a documented cross-module constraint. Risk: low in practice because
 * the name is set by approveEnrollment callers and the ceremony uses "device1"
 * for the first real device.
 */

import {
  verifyDeviceSet,
  parseEntryBody,
  type DeviceSetLog,
  type DeviceRef,
  type OwnerRoot,
} from "./deviceset";

export interface DeviceListItem {
  /** X25519 public key (base64) — unique identifier within the member set. */
  x25519Pub: string;
  /** ECDSA-P256 signing public key (base64 SEC1 uncompressed). */
  signPub: string;
  /** Authenticated device name (from signed body, never self-asserted UI input). */
  name: string;
  /**
   * Enrollment timestamp as a decimal nanoseconds-since-Unix-epoch string (WM10).
   * BigInt-string, not Date — callers convert to Date for display.
   */
  enrolledAt: string;
  /** sign_pub of the member who signed the add entry for this device; null for genesis device1 and recovery. */
  enrolledBySignPub: string | null;
  /** True if this device is the current browser device. */
  isCurrent: boolean;
  /** True if this is the recovery virtual device (not a real enrolled browser). */
  isRecovery: boolean;
}

/** buildDeviceList builds and returns a verified, annotated device list. */
export async function buildDeviceList(
  log: DeviceSetLog,
  ownerRoot: OwnerRoot,
  opts: {
    /** The current device's ref — used to flag isCurrent. */
    currentSignPub?: string;
    /** Pinned head version for regression check ([WM6]). */
    pinnedHeadVersion?: number;
  } = {},
): Promise<DeviceListItem[]> {
  // Fail-closed: rethrows on any chain violation.
  const { members } = await verifyDeviceSet(log, ownerRoot, opts.pinnedHeadVersion);

  // Build a map: sign_pub → {name, enrolledAt, enrolledBySignPub, isRecovery}
  // by replaying the log entries.
  const infoMap = new Map<
    string,
    { name: string; enrolledAt: string; enrolledBySignPub: string | null; isRecovery: boolean }
  >();

  // Genesis entry: device1 is devices[0], recovery is devices[1].
  const genesisEntry = log.entries[0];
  const genesisBody = parseEntryBody(genesisEntry);
  if (genesisBody.type === "genesis" && genesisBody.devices.length >= 1) {
    const d1 = genesisBody.devices[0];
    const d1Name = genesisBody.label?.name ?? "device1";
    const d1At = genesisBody.label?.enrolled_at ?? "0";
    infoMap.set(d1.sign_pub, {
      name: d1Name,
      enrolledAt: d1At,
      enrolledBySignPub: null,
      isRecovery: false,
    });

    if (genesisBody.devices.length >= 2) {
      const rec = genesisBody.devices[1];
      const recName = genesisBody.recovery_label?.name ?? "recovery";
      const recAt = genesisBody.recovery_label?.enrolled_at ?? "0";
      infoMap.set(rec.sign_pub, {
        name: recName,
        enrolledAt: recAt,
        enrolledBySignPub: null,
        // genesis devices[1] with matching recovery_sign_pub is the recovery device
        isRecovery: rec.sign_pub === ownerRoot.recovery_sign_pub,
      });
    }
  }

  // Walk remaining entries (add/remove).
  for (let i = 1; i < log.entries.length; i++) {
    const entry = log.entries[i];
    const body = parseEntryBody(entry);

    if (body.type === "add" && body.change) {
      const name = body.label?.name ?? "unknown";
      const enrolledAt = body.label?.enrolled_at ?? "0";
      // enrolledBySignPub: the member who signed this add entry
      const signerPub = entry.sigs.length > 0 ? entry.sigs[0].signer_pub : null;
      // The recovery virtual device added by recovery.ts uses the reserved name "recovery"
      const isRecovery = name === "recovery";
      infoMap.set(body.change.sign_pub, {
        name,
        enrolledAt,
        enrolledBySignPub: signerPub,
        isRecovery,
      });
    }
    // remove entries do not need to update the map — removed devices are no longer members
  }

  // Build the final list from the verified member set.
  return members.map((m) => {
    const info = infoMap.get(m.sign_pub) ?? {
      name: "unknown",
      enrolledAt: "0",
      enrolledBySignPub: null,
      isRecovery: false,
    };
    return {
      x25519Pub: m.x25519_pub,
      signPub: m.sign_pub,
      name: info.name,
      enrolledAt: info.enrolledAt,
      enrolledBySignPub: info.enrolledBySignPub,
      isCurrent: opts.currentSignPub !== undefined && m.sign_pub === opts.currentSignPub,
      isRecovery: info.isRecovery,
    };
  });
}

/**
 * cascadeForDevice computes the transitive closure of devices enrolled by
 * the target (the target's direct enrollees, plus enrollees of enrollees, etc.).
 *
 * [WM7] Distinct from enrollment.ts's single-level findCascadeDevices — this
 * is a full transitive closure for the revocation cascade prompt.
 */
export function cascadeForDevice(log: DeviceSetLog, targetSignPub: string): DeviceRef[] {
  // Build an adjacency map: signer_sign_pub → [enrolled DeviceRef, ...]
  const enrolled = new Map<string, DeviceRef[]>();
  for (const entry of log.entries) {
    const body = parseEntryBody(entry);
    if (body.type !== "add" || !body.change) continue;
    for (const sig of entry.sigs) {
      const list = enrolled.get(sig.signer_pub) ?? [];
      list.push(body.change);
      enrolled.set(sig.signer_pub, list);
    }
  }

  // BFS/DFS transitive closure starting from targetSignPub.
  const visited = new Set<string>();
  const result: DeviceRef[] = [];
  const queue: string[] = [targetSignPub];

  while (queue.length > 0) {
    const current = queue.shift()!;
    const children = enrolled.get(current) ?? [];
    for (const child of children) {
      if (!visited.has(child.sign_pub)) {
        visited.add(child.sign_pub);
        result.push(child);
        queue.push(child.sign_pub);
      }
    }
  }

  return result;
}
