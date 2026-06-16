import { useCallback, useState } from "react";
import { listMigrationTargets, type MigrationTarget } from "@/api/migration";
import { ForkError, runFork, runForkDelivery, type ForkProgressStep, type ForkResult } from "@/api/fork";
import { loadDeviceKeys } from "@/keys/device";
import { PINNED_ROOT_CA_PEM } from "@/config/trustAnchors";

export type ForkPhase =
  | "idle"
  | "loading"
  | "selecting"
  | "confirming"
  | "running"
  | "done"
  | "delivery-pending"
  | "needs-enroll"
  | "reconnecting";

export interface ForkSpawnState {
  phase: ForkPhase;
  sourceSpawnId: string | null;
  forkSpawnId: string | null;
  targets: MigrationTarget[];
  selectedTarget: MigrationTarget | null;
  name: string;
  progress: ForkProgressStep | null;
  result: ForkResult | null;
  errorMsg: string | null;
}

export interface ForkSpawnActions {
  open: (sourceSpawnId: string) => Promise<void>;
  select: (target: MigrationTarget) => void;
  setName: (name: string) => void;
  confirm: () => Promise<void>;
  cancel: () => void;
  openDeliveryPending: (forkSpawnId: string) => void;
  retryDelivery: () => Promise<void>;
}

const IDLE: ForkSpawnState = {
  phase: "idle",
  sourceSpawnId: null,
  forkSpawnId: null,
  targets: [],
  selectedTarget: null,
  name: "",
  progress: null,
  result: null,
  errorMsg: null,
};

function defaultTarget(targets: MigrationTarget[]): MigrationTarget | null {
  return targets.find((t) => t.isCurrent && t.online)
    ?? targets.find((t) => t.online)
    ?? targets[0]
    ?? null;
}

function targetInput(target: MigrationTarget | null) {
  if (!target || target.isCurrent) return {};
  if (target.class === "cloud") return { class: target.class };
  return { nodeId: target.nodeId };
}

function isEnrollmentError(message: string): boolean {
  return /enroll|device keys|required/i.test(message);
}

export function useForkSpawn(): { state: ForkSpawnState } & ForkSpawnActions {
  const [state, setState] = useState<ForkSpawnState>(IDLE);

  const open = useCallback(async (sourceSpawnId: string) => {
    setState({ ...IDLE, phase: "loading", sourceSpawnId });
    try {
      const data = await listMigrationTargets(sourceSpawnId);
      const selectedTarget = defaultTarget(data.targets);
      setState((s) => ({
        ...s,
        phase: "selecting",
        targets: data.targets,
        selectedTarget,
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

  const setName = useCallback((name: string) => {
    setState((s) => ({ ...s, name }));
  }, []);

  const confirm = useCallback(async () => {
    const { sourceSpawnId, selectedTarget, name } = state;
    if (!sourceSpawnId) return;

    setState((s) => ({ ...s, phase: "running", progress: "forking", errorMsg: null }));
    try {
      const result = await runFork(
        sourceSpawnId,
        targetInput(selectedTarget),
        loadDeviceKeys,
        PINNED_ROOT_CA_PEM,
        new Date(),
        name,
        (step) => setState((s) => ({ ...s, progress: step })),
      );
      setState((s) => ({
        ...s,
        phase: "done",
        forkSpawnId: result.forkSpawnId,
        result,
        progress: null,
      }));
    } catch (e: unknown) {
      if (e instanceof ForkError && e.leg === "delivery") {
        setState((s) => ({
          ...s,
          phase: isEnrollmentError(e.message) ? "needs-enroll" : "delivery-pending",
          forkSpawnId: e.forkSpawnId || s.forkSpawnId,
          errorMsg: e.message,
          progress: null,
        }));
        return;
      }
      if (e instanceof ForkError && e.leg === "network") {
        setState((s) => ({ ...s, phase: "reconnecting", errorMsg: e.message, progress: null }));
        return;
      }
      setState((s) => ({
        ...s,
        phase: "reconnecting",
        errorMsg: e instanceof Error ? e.message : String(e),
        progress: null,
      }));
    }
  }, [state]);

  const cancel = useCallback(() => {
    setState(IDLE);
  }, []);

  const openDeliveryPending = useCallback((forkSpawnId: string) => {
    setState({
      ...IDLE,
      phase: "delivery-pending",
      forkSpawnId,
      errorMsg: "Journal key delivery is pending for this fork.",
    });
  }, []);

  const retryDelivery = useCallback(async () => {
    const forkSpawnId = state.forkSpawnId;
    if (!forkSpawnId) return;

    setState((s) => ({ ...s, phase: "running", progress: "verifying-node", errorMsg: null }));
    try {
      const data = await listMigrationTargets(forkSpawnId);
      const current = data.targets.find((t) => t.isCurrent && t.online) ?? data.targets.find((t) => t.online);
      if (!current) {
        setState((s) => ({
          ...s,
          phase: "delivery-pending",
          progress: null,
          errorMsg: "No online target is available for delivery retry.",
        }));
        return;
      }
      const deviceKeys = await loadDeviceKeys();
      if (!deviceKeys) {
        setState((s) => ({ ...s, phase: "needs-enroll", progress: null }));
        return;
      }
      const delivered = await runForkDelivery(
        forkSpawnId,
        current,
        deviceKeys,
        PINNED_ROOT_CA_PEM,
        new Date(),
        (step) => setState((s) => ({ ...s, progress: step })),
      );
      setState((s) => ({
        ...s,
        phase: "done",
        result: {
          forkSpawnId,
          resolvedNodeId: current.nodeId,
          transferSetId: "",
          journalKeysDelivered: delivered.journalKeysDelivered,
        },
        progress: null,
      }));
    } catch (e: unknown) {
      if (e instanceof ForkError && e.leg === "delivery") {
        setState((s) => ({
          ...s,
          phase: "delivery-pending",
          forkSpawnId: e.forkSpawnId || s.forkSpawnId,
          errorMsg: e.message,
          progress: null,
        }));
        return;
      }
      setState((s) => ({
        ...s,
        phase: "reconnecting",
        errorMsg: e instanceof Error ? e.message : String(e),
        progress: null,
      }));
    }
  }, [state.forkSpawnId]);

  return { state, open, select, setName, confirm, cancel, openDeliveryPending, retryDelivery };
}
