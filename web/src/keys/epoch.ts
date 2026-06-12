/**
 * Re-seal epoch tracking (Phase 5/6, [WM2]).
 *
 * Enrollment and revocation re-seals are not atomic across N secrets. Each
 * ciphertext is stamped with the device-set version it was sealed under (the
 * re-seal epoch, implied by the head-hash AAD). A removal is INCOMPLETE until
 * every secret's epoch >= the removal entry's version.
 *
 * This module persists sweep progress in localStorage (simple for MVP; can be
 * moved to IndexedDB if the secret count grows large). Progress survives
 * browser restarts.
 *
 * The UI shows "revocation in progress — N secrets still openable by the
 * removed device" until the sweep completes ([WM2]).
 */

export interface SweepProgress {
  /** The device-set version that triggered this sweep (removal or add). */
  targetVersion: number;
  /** Total number of secrets that need re-sealing. */
  total: number;
  /** Number of secrets that have been re-sealed so far. */
  done: number;
  /** Full list of secret IDs to re-seal (needed for resume after restart). */
  secretIds: string[];
  /** Secret IDs that have been successfully re-sealed. */
  completed: string[];
  /** Secret IDs that failed (will be retried on resume). */
  failed: string[];
  /** Whether the sweep was triggered by a removal (vs. add). */
  isRevocation: boolean;
  /** The x25519_pub of the removed device (revocation sweeps only). */
  revokedX25519Pub?: string;
  /** Timestamp of last progress update (ms since epoch). */
  updatedAt: number;
}

const EPOCH_STORAGE_KEY = "spawnery:seal-epoch-sweep";

/** saveSweepProgress persists current sweep state to localStorage. */
export function saveSweepProgress(progress: SweepProgress): void {
  localStorage.setItem(EPOCH_STORAGE_KEY, JSON.stringify(progress));
}

/** loadSweepProgress restores interrupted sweep state, or null if no sweep is active. */
export function loadSweepProgress(): SweepProgress | null {
  const raw = localStorage.getItem(EPOCH_STORAGE_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as SweepProgress;
  } catch {
    return null;
  }
}

/** clearSweepProgress removes the persisted sweep state (sweep complete or abandoned). */
export function clearSweepProgress(): void {
  localStorage.removeItem(EPOCH_STORAGE_KEY);
}

/** isSweepComplete returns true if all secrets have been re-sealed. */
export function isSweepComplete(progress: SweepProgress): boolean {
  return progress.done >= progress.total;
}

/**
 * remainingCount returns the number of secrets still openable by the removed
 * device (relevant for revocation sweeps, displayed in the UI).
 */
export function remainingCount(progress: SweepProgress): number {
  return Math.max(0, progress.total - progress.done);
}

/**
 * markSecretsCompleted marks a batch of secret IDs as successfully re-sealed,
 * updates progress, and saves.
 */
export function markSecretsCompleted(
  progress: SweepProgress,
  secretIds: string[],
): SweepProgress {
  const next: SweepProgress = {
    ...progress,
    completed: [...progress.completed, ...secretIds],
    done: progress.done + secretIds.length,
    updatedAt: Date.now(),
  };
  saveSweepProgress(next);
  return next;
}

/**
 * markSecretFailed records a secret ID as failed for retry. Does not update
 * done count — failure means the sweep is not complete.
 */
export function markSecretFailed(
  progress: SweepProgress,
  secretId: string,
): SweepProgress {
  const next: SweepProgress = {
    ...progress,
    failed: [...progress.failed, secretId],
    updatedAt: Date.now(),
  };
  saveSweepProgress(next);
  return next;
}

/**
 * initSweep creates and saves a new SweepProgress for a device-set mutation.
 */
export function initSweep(opts: {
  targetVersion: number;
  secretIds: string[];
  isRevocation: boolean;
  revokedX25519Pub?: string;
}): SweepProgress {
  const progress: SweepProgress = {
    targetVersion: opts.targetVersion,
    total: opts.secretIds.length,
    done: 0,
    secretIds: opts.secretIds,
    completed: [],
    failed: [],
    isRevocation: opts.isRevocation,
    revokedX25519Pub: opts.revokedX25519Pub,
    updatedAt: Date.now(),
  };
  saveSweepProgress(progress);
  return progress;
}
