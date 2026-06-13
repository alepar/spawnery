/**
 * useMoveTo — state machine hook for the Move-to migration UI (sp-8dkp Phase 8).
 *
 * States:
 *   idle             — not running
 *   loading          — fetching migration targets + journal entries + classifying durability
 *   selecting        — presenting the list of available target nodes (preflight gate)
 *   needs-enroll     — owner-sealed spawn but browser not enrolled; show enroll/approve prompt
 *   upgrading        — node-local upgrade to owner-sealed in progress
 *   confirming       — user selected a target; awaiting user's "Migrate" click
 *   running          — migration in progress (substep tracked in progress field)
 *   done             — migration completed successfully
 *   error-suspend    — suspend leg failed; spawn in error state; UI offers Recreate
 *   error-resume     — resume leg failed; spawn back in suspended state; UI offers Resume-on-origin
 *   delivery-pending — delivery leg failed; persistent reload-derivable; UI offers Retry Delivery
 *   reconnecting     — CP unreachable mid-operation; UI shows retry banner
 *
 * Minimize (WM14): after phase 1 (running/suspend step starts) the modal can minimise
 * to a badge via minimize(); restore() returns to the full modal.
 *
 * Delivery-pending reconstruction (spec §3): call openDeliveryPending(spawnId) when
 * a spawn has journalKeyDeliveryPending=true in its status — the modal opens directly
 * in delivery-pending so the user can retry without knowing a migration was in progress.
 *
 * Usage:
 *   const moveTo = useMoveTo();
 *   moveTo.open("spawn-id");            // load targets for a spawn
 *   moveTo.select(target);              // user picks a target row
 *   moveTo.confirm();                   // user confirms; triggers upgrade (if node-local) + runMigrate
 *   moveTo.cancel();                    // dismiss / reset to idle
 *   moveTo.minimize();                  // collapse to badge (available from running phase)
 *   moveTo.restore();                   // expand badge back to full modal
 *   moveTo.openDeliveryPending(id);     // open directly in delivery-pending state
 *   moveTo.retryDelivery();             // retry delivery from delivery-pending state
 */

import { useState, useCallback } from "react";
import {
  listMigrationTargets,
  getJournalKeyCiphertext,
  classifyDurability,
  runMigrate,
  upgradeToOwnerSealed,
  MigrateError,
  type MigrationTarget,
  type JournalEntry,
  type DurabilityClass,
  type MigrateProgress,
  type MigrateResult,
} from "@/api/migration";
import { loadDeviceKeys } from "@/keys/device";
import { PINNED_ROOT_CA_PEM } from "@/config/trustAnchors";

// ── State shape ───────────────────────────────────────────────────────────────

export type MoveToPhase =
  | "idle"
  | "loading"
  | "selecting"
  | "needs-enroll"
  | "upgrading"
  | "confirming"
  | "running"
  | "done"
  | "error-suspend"
  | "error-resume"
  | "delivery-pending"
  | "reconnecting";

export interface MoveToState {
  phase:           MoveToPhase;
  spawnId:         string | null;
  targets:         MigrationTarget[];
  entries:         JournalEntry[];
  durability:      DurabilityClass | null;
  selectedTarget:  MigrationTarget | null;
  progress:        MigrateProgress | null;
  result:          MigrateResult | null;
  errorMsg:        string | null;
  minimized:       boolean;
}

export interface MoveToActions {
  /** Begin the migration flow for a spawn: loads targets + classifies durability. */
  open: (spawnId: string) => void;
  /** User selects a target from the list; advances to confirming phase. */
  select: (target: MigrationTarget) => void;
  /** User confirms the migration; runs upgrade (if needed) then runMigrate. */
  confirm: () => void;
  /** Cancel / dismiss. */
  cancel: () => void;
  /** Collapse to badge (WM14). Available once migration has started (running phase). */
  minimize: () => void;
  /** Expand badge back to full modal. */
  restore: () => void;
  /** Open directly in delivery-pending state (reload reconstruction, spec §3). */
  openDeliveryPending: (spawnId: string) => void;
  /** Retry the delivery leg from delivery-pending state. */
  retryDelivery: () => void;
}

// ── Initial state ─────────────────────────────────────────────────────────────

const IDLE: MoveToState = {
  phase: "idle",
  spawnId: null,
  targets: [],
  entries: [],
  durability: null,
  selectedTarget: null,
  progress: null,
  result: null,
  errorMsg: null,
  minimized: false,
};

// ── Helpers ───────────────────────────────────────────────────────────────────

/** Classify a MigrateError leg into the corresponding MoveToPhase. */
function legToPhase(leg: MigrateError["leg"]): MoveToPhase {
  switch (leg) {
    case "suspend":  return "error-suspend";
    case "resume":   return "error-resume";
    case "delivery": return "delivery-pending";
    case "network":  return "reconnecting";
  }
}

// ── Hook ──────────────────────────────────────────────────────────────────────

export function useMoveTo(): { state: MoveToState } & MoveToActions {
  const [state, setState] = useState<MoveToState>(IDLE);

  const open = useCallback(async (spawnId: string) => {
    setState({ ...IDLE, phase: "loading", spawnId });
    try {
      const [targetsData, entries] = await Promise.all([
        listMigrationTargets(spawnId),
        getJournalKeyCiphertext(spawnId),
      ]);
      const durability = classifyDurability(entries, targetsData.targets, targetsData.spawnDurabilityClass);
      // Exclude the current node (can't migrate to where you already are).
      const selectable = targetsData.targets.filter((t) => !t.isCurrent);
      setState((s) => ({
        ...s,
        phase: "selecting",
        targets: selectable,
        entries,
        durability,
      }));
    } catch (e: unknown) {
      setState((s) => ({
        ...s,
        phase: "reconnecting",
        errorMsg: e instanceof Error ? e.message : String(e),
      }));
    }
  }, []);

  const select = useCallback((target: MigrationTarget) => {
    setState((s) => ({
      ...s,
      phase: "confirming",
      selectedTarget: target,
    }));
  }, []);

  // confirm reads spawnId/selectedTarget/durability from state.
  const confirm = useCallback(async () => {
    const { spawnId, selectedTarget, durability } = state;
    if (!spawnId || !selectedTarget) return;

    // Preflight: enrollment check (spec §3 "Enrollment check for owner-sealed spawns").
    // For owner-sealed spawns the browser must be enrolled to unseal the journal key.
    if (durability === "owner-sealed") {
      setState((s) => ({ ...s, phase: "running", progress: { step: "fetching-keys" } }));
      const deviceKeys = await loadDeviceKeys();
      if (!deviceKeys) {
        setState((s) => ({ ...s, phase: "needs-enroll", progress: null }));
        return;
      }
      // Run the full migration (delivery included).
      try {
        const result = await runMigrate(
          spawnId,
          selectedTarget,
          deviceKeys,
          PINNED_ROOT_CA_PEM,
          new Date(),
          (p) => setState((s) => ({ ...s, progress: p })),
        );
        setState((s) => ({ ...s, phase: "done", result, progress: null }));
      } catch (e: unknown) {
        if (e instanceof MigrateError) {
          setState((s) => ({
            ...s,
            phase: legToPhase(e.leg),
            errorMsg: e.message,
            progress: null,
          }));
        } else {
          setState((s) => ({
            ...s,
            phase: "error-suspend",
            errorMsg: e instanceof Error ? e.message : String(e),
            progress: null,
          }));
        }
      }
      return;
    }

    // node-local: upgrade to owner-sealed first, then migrate.
    if (durability === "node-local") {
      setState((s) => ({ ...s, phase: "upgrading", progress: null }));
      const deviceKeys = await loadDeviceKeys();
      if (!deviceKeys) {
        setState((s) => ({ ...s, phase: "needs-enroll", progress: null }));
        return;
      }
      try {
        // Export the raw X25519 pubkey to pass to the CP (proto bytes field).
        const xPubRaw = new Uint8Array(
          await crypto.subtle.exportKey("raw", deviceKeys.x25519Public),
        );
        await upgradeToOwnerSealed(spawnId, [xPubRaw]);
      } catch (e: unknown) {
        setState((s) => ({
          ...s,
          phase: "reconnecting",
          errorMsg: `Upgrade failed: ${e instanceof Error ? e.message : String(e)}`,
          progress: null,
        }));
        return;
      }
      // Upgrade succeeded — reload targets + entries now that durability has changed.
      setState((s) => ({ ...s, phase: "running", progress: { step: "fetching-keys" } }));
      try {
        const result = await runMigrate(
          spawnId,
          selectedTarget,
          deviceKeys,
          PINNED_ROOT_CA_PEM,
          new Date(),
          (p) => setState((s) => ({ ...s, progress: p })),
        );
        setState((s) => ({ ...s, phase: "done", result, progress: null }));
      } catch (e: unknown) {
        if (e instanceof MigrateError) {
          setState((s) => ({
            ...s,
            phase: legToPhase(e.leg),
            errorMsg: e.message,
            progress: null,
          }));
        } else {
          setState((s) => ({
            ...s,
            phase: "error-suspend",
            errorMsg: e instanceof Error ? e.message : String(e),
            progress: null,
          }));
        }
      }
      return;
    }

    // ephemeral: no journal keys; pure lifecycle migration.
    setState((s) => ({ ...s, phase: "running", progress: { step: "migrating" } }));
    try {
      // runMigrate with empty device keys is safe for ephemeral (no entries to unseal).
      const deviceKeys = await loadDeviceKeys();
      // loadDeviceKeys may be null for ephemeral spawns — that is OK; runMigrate
      // will skip the delivery leg because there are no journal entries.
      const result = await runMigrate(
        spawnId,
        selectedTarget,
        deviceKeys!,           // may be null but runMigrate exits early when entries=[].
        PINNED_ROOT_CA_PEM,
        new Date(),
        (p) => setState((s) => ({ ...s, progress: p })),
      );
      setState((s) => ({ ...s, phase: "done", result, progress: null }));
    } catch (e: unknown) {
      if (e instanceof MigrateError) {
        setState((s) => ({
          ...s,
          phase: legToPhase(e.leg),
          errorMsg: e.message,
          progress: null,
        }));
      } else {
        setState((s) => ({
          ...s,
          phase: "error-suspend",
          errorMsg: e instanceof Error ? e.message : String(e),
          progress: null,
        }));
      }
    }
  }, [state.spawnId, state.selectedTarget, state.durability]); // eslint-disable-line react-hooks/exhaustive-deps

  const cancel = useCallback(() => {
    setState(IDLE);
  }, []);

  const minimize = useCallback(() => {
    setState((s) => ({ ...s, minimized: true }));
  }, []);

  const restore = useCallback(() => {
    setState((s) => ({ ...s, minimized: false }));
  }, []);

  /** Open directly in delivery-pending state for reload reconstruction (spec §3). */
  const openDeliveryPending = useCallback((spawnId: string) => {
    setState({
      ...IDLE,
      phase: "delivery-pending",
      spawnId,
      errorMsg: "Journal key not yet delivered — retry from an enrolled device.",
    });
  }, []);

  /** Retry delivery from delivery-pending state. */
  const retryDelivery = useCallback(async () => {
    const { spawnId } = state;
    if (!spawnId) return;

    const deviceKeys = await loadDeviceKeys();
    if (!deviceKeys) {
      setState((s) => ({ ...s, phase: "needs-enroll", progress: null }));
      return;
    }

    // Re-open to reload the target list and try delivery again.
    // We don't know the originally selected target from the persisted delivery-pending state,
    // so we redirect the user to target selection to pick again (safe: migration already done).
    setState((s) => ({ ...s, phase: "loading", errorMsg: null }));
    try {
      const [targetsData, entries] = await Promise.all([
        listMigrationTargets(spawnId),
        getJournalKeyCiphertext(spawnId),
      ]);
      // If the CP reports no pending entries the delivery already completed on another device.
      if (entries.length === 0) {
        setState((s) => ({
          ...s,
          phase: "done",
          result: { resolvedNodeId: "", journalKeysDelivered: 0, transferSetId: "" },
        }));
        return;
      }
      const durability = classifyDurability(entries, targetsData.targets, targetsData.spawnDurabilityClass);
      const selectable = targetsData.targets.filter((t) => !t.isCurrent);
      setState((s) => ({
        ...s,
        phase: "selecting",
        targets: selectable,
        entries,
        durability,
      }));
    } catch (e: unknown) {
      setState((s) => ({
        ...s,
        phase: "reconnecting",
        errorMsg: e instanceof Error ? e.message : String(e),
      }));
    }
  }, [state.spawnId]); // eslint-disable-line react-hooks/exhaustive-deps

  return { state, open, select, confirm, cancel, minimize, restore, openDeliveryPending, retryDelivery };
}
