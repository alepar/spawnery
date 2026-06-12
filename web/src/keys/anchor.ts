/**
 * Local trust-anchor persistence ([WM5][WM6]).
 *
 * The OwnerRoot (device1_sign_pub + recovery_sign_pub) and the pinned head
 * version must be persisted locally. verifyDeviceSet is fail-closed: it
 * requires a locally-pinned anchor and MUST NOT TOFU it from the AS.
 *
 * Producers (write once each):
 *   - completeCeremony  → saveAnchor({ownerRoot, headVersion: 1})
 *   - finalizeEnrollment → saveAnchor({ownerRoot, headVersion: approval.headVersion})
 *   - recoverAndRotate   → bumpPinnedHead(headVersion+3) — OwnerRoot unchanged
 *     (recovery sign_pub changes in the chain but the genesis-pinned root stays;
 *      this is safe because verifyDeviceSet uses the pinned genesis pubkeys to
 *      verify the co-signatures on the genesis entry only, not on later entries)
 *
 * [WM6] Monotonic guard: bumpPinnedHead never regresses the pinned version.
 */

import type { OwnerRoot } from "./deviceset";

export interface DeviceAnchor {
  ownerRoot: OwnerRoot;
  headVersion: number;
}

const ANCHOR_STORAGE_KEY = "spawnery:device-anchor";

/** saveAnchor persists the owner root and head version to localStorage. */
export function saveAnchor(anchor: DeviceAnchor): void {
  localStorage.setItem(ANCHOR_STORAGE_KEY, JSON.stringify(anchor));
}

/** loadAnchor restores the persisted anchor, or returns null if absent. */
export function loadAnchor(): DeviceAnchor | null {
  const raw = localStorage.getItem(ANCHOR_STORAGE_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as DeviceAnchor;
  } catch {
    return null;
  }
}

/** clearAnchor removes the persisted anchor (e.g. on self-revocation). */
export function clearAnchor(): void {
  localStorage.removeItem(ANCHOR_STORAGE_KEY);
}

/**
 * bumpPinnedHead updates the persisted head version monotonically ([WM6]).
 * Throws if no anchor exists or if newVersion regresses the current pinned head.
 */
export function bumpPinnedHead(newVersion: number): void {
  const anchor = loadAnchor();
  if (!anchor) {
    throw new Error("anchor: cannot bump head — no anchor persisted");
  }
  if (newVersion < anchor.headVersion) {
    throw new Error(
      `anchor: head regression rejected — pinned ${anchor.headVersion} but bump to ${newVersion}`,
    );
  }
  saveAnchor({ ...anchor, headVersion: newVersion });
}
