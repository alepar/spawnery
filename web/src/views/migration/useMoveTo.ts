/**
 * useMoveTo — state machine hook for the Move-to migration UI (sp-8dkp Phase 8).
 *
 * States:
 *   idle       — not running
 *   loading    — fetching migration targets + journal entries + classifying durability
 *   selecting  — presenting the list of available target nodes
 *   confirming — user selected a target; awaiting user's "Migrate" click
 *   running    — migration in progress (substep tracked in progress field)
 *   done       — migration completed successfully
 *   error      — migration failed; error message in errorMsg
 *
 * Usage:
 *   const moveTo = useMoveTo();
 *   moveTo.open("spawn-id");   // load targets for a spawn
 *   moveTo.select(target);     // user picks a target row
 *   moveTo.confirm();          // user confirms; triggers runMigrate
 *   moveTo.cancel();           // dismiss / reset to idle
 */

import { useState, useCallback } from "react";
import {
  listMigrationTargets,
  getJournalKeyCiphertext,
  classifyDurability,
  runMigrate,
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
  | "confirming"
  | "running"
  | "done"
  | "error";

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
}

export interface MoveToActions {
  /** Begin the migration flow for a spawn: loads targets + classifies durability. */
  open: (spawnId: string) => void;
  /** User selects a target from the list; advances to confirming phase. */
  select: (target: MigrationTarget) => void;
  /** User confirms the migration; runs runMigrate. */
  confirm: () => void;
  /** Cancel / dismiss. */
  cancel: () => void;
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
};

// ── Hook ──────────────────────────────────────────────────────────────────────

export function useMoveTo(): { state: MoveToState } & MoveToActions {
  const [state, setState] = useState<MoveToState>(IDLE);

  const open = useCallback(async (spawnId: string) => {
    setState({ ...IDLE, phase: "loading", spawnId });
    try {
      const [targets, entries] = await Promise.all([
        listMigrationTargets(spawnId),
        getJournalKeyCiphertext(spawnId),
      ]);
      const durability = classifyDurability(entries, targets);
      // Exclude the current node (can't migrate to where you already are).
      const selectable = targets.filter((t) => !t.isCurrent);
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
        phase: "error",
        errorMsg: e instanceof Error ? e.message : String(e),
      }));
    }
  }, []); // no state deps: spawnId comes from the argument

  const select = useCallback((target: MigrationTarget) => {
    setState((s) => ({
      ...s,
      phase: "confirming",
      selectedTarget: target,
    }));
  }, []);

  // confirm reads spawnId/selectedTarget from state. Include them in the dep array
  // so the callback always closes over the latest values (the button is only visible
  // in "confirming" phase so this does not cause excessive recreation).
  const confirm = useCallback(async () => {
    const { spawnId, selectedTarget } = state;
    if (!spawnId || !selectedTarget) return;

    setState((s) => ({ ...s, phase: "running", progress: { step: "fetching-keys" } }));

    try {
      const deviceKeys = await loadDeviceKeys();
      if (!deviceKeys) {
        throw new Error(
          "Device keys not found. This browser is not enrolled; use spawnctl move or enroll a device first.",
        );
      }

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
      setState((s) => ({
        ...s,
        phase: "error",
        errorMsg: e instanceof Error ? e.message : String(e),
        progress: null,
      }));
    }
  }, [state.spawnId, state.selectedTarget]); // eslint-disable-line react-hooks/exhaustive-deps

  const cancel = useCallback(() => {
    setState(IDLE);
  }, []);

  return { state, open, select, confirm, cancel };
}
