import { unary } from "./connect";
import {
  deliverOwnerSealedJournalKeys,
  getJournalKeyCiphertext,
  type MigrationTarget,
  type OwnerSealedDeliveryResult,
} from "./migration";
import { authEnabled, useSessionStore } from "@/auth/session";
import { pollAndSign, registerPendedOp, clearPendedOp } from "@/auth/intent";
import type { DeviceKeys } from "@/keys/device";

export class ForkError extends Error {
  constructor(
    message: string,
    public readonly leg: "fork" | "delivery" | "network",
    public readonly forkSpawnId = "",
  ) {
    super(message);
    this.name = "ForkError";
  }
}

export interface ForkTargetInput {
  nodeId?: string;
  class?: string;
}

export interface ForkResult {
  forkSpawnId: string;
  resolvedNodeId: string;
  transferSetId: string;
  journalKeysDelivered: number;
}

export type ForkProgressStep = "forking" | "verifying-node" | "resealing" | "delivering" | "done";
export type ForkDeviceKeysInput = DeviceKeys | null | (() => Promise<DeviceKeys | null>);

async function resolveDeviceKeys(input: ForkDeviceKeysInput): Promise<DeviceKeys | null> {
  if (typeof input === "function") return input();
  return input;
}

export async function runFork(
  sourceSpawnId: string,
  target: ForkTargetInput,
  deviceKeys: ForkDeviceKeysInput,
  rootPEM: string,
  now: Date,
  name = "",
  onProgress?: (step: ForkProgressStep) => void,
): Promise<ForkResult> {
  const sourceID = sourceSpawnId.trim();
  const targetNodeId = (target.nodeId ?? "").trim();
  const targetClass = (target.class ?? "").trim();
  const forkName = name.trim();
  if (targetNodeId && targetClass) {
    throw new ForkError("Specify node or class, not both", "fork");
  }

  onProgress?.("forking");

  // ForkSpawn blocks at the CP awaiting the client's SignedIntent for the fork-spawn op. The CP
  // registers the pending intent under the SOURCE spawn id, so pollAndSign MUST run concurrently
  // with the RPC (kicking it off after the await would deadlock — CP waits for the intent, client
  // waits for the RPC). Mirrors createSpawn/resumeSpawn in spawnlet.ts. The tuple's spawnId is the
  // CP-minted fork id, which we can't know yet; _validateTuple skips the spawnId check for fork.
  if (authEnabled()) {
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    const pended = { op: "fork-spawn", spawnId: sourceID };
    registerPendedOp(pended);
    pollAndSign({ spawnId: sourceID, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("fork intent sign failed:", e))
      .finally(() => clearPendedOp(sourceID));
  }

  let forkSpawnId: string;
  let resolvedNodeId: string;
  let transferSetId: string;
  try {
    const r = await unary<{ forkSpawnId?: string; nodeId?: string; transferSetId?: string }>("ForkSpawn", {
      spawnId: sourceID,
      targetNodeId,
      targetClass,
      name: forkName,
    });
    forkSpawnId = r.forkSpawnId ?? "";
    resolvedNodeId = r.nodeId ?? "";
    transferSetId = r.transferSetId ?? "";
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Fork failed: ${msg}`, "fork");
  }

  let entries;
  try {
    entries = await getJournalKeyCiphertext(forkSpawnId);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Failed to fetch journal key ciphertext for fork ${forkSpawnId}: ${msg}`, "delivery", forkSpawnId);
  }
  if (entries.length === 0) {
    onProgress?.("done");
    return { forkSpawnId, resolvedNodeId, transferSetId, journalKeysDelivered: 0 };
  }
  let resolvedDeviceKeys: DeviceKeys | null;
  try {
    resolvedDeviceKeys = await resolveDeviceKeys(deviceKeys);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Fork ${forkSpawnId} created, but device keys could not be loaded: ${msg}`, "delivery", forkSpawnId);
  }
  if (!resolvedDeviceKeys) {
    throw new ForkError(`Device enrollment required to deliver journal keys for fork ${forkSpawnId}`, "delivery", forkSpawnId);
  }

  try {
    const delivered = await deliverOwnerSealedJournalKeys(
      forkSpawnId,
      entries,
      {
        nodeId: resolvedNodeId || targetNodeId,
        class: targetClass,
      } satisfies Pick<MigrationTarget, "nodeId" | "class">,
      resolvedDeviceKeys,
      rootPEM,
      now,
      (step) => onProgress?.(step),
      undefined,
    );
    onProgress?.("done");
    return { forkSpawnId, resolvedNodeId, transferSetId, journalKeysDelivered: delivered.journalKeysDelivered };
  } catch (e: unknown) {
    if (e instanceof ForkError) throw e;
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Fork ${forkSpawnId} created, but journal key delivery is pending: ${msg}`, "delivery", forkSpawnId);
  }
}

export async function runForkDelivery(
  forkSpawnId: string,
  target: Pick<MigrationTarget, "nodeId" | "class">,
  deviceKeys: DeviceKeys,
  rootPEM: string,
  now: Date,
  onProgress?: (step: "verifying-node" | "resealing" | "delivering") => void,
): Promise<OwnerSealedDeliveryResult> {
  let entries;
  try {
    entries = await getJournalKeyCiphertext(forkSpawnId);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Failed to fetch journal key ciphertext for fork ${forkSpawnId}: ${msg}`, "delivery", forkSpawnId);
  }
  if (entries.length === 0) {
    return { journalKeysDelivered: 0 };
  }
  try {
    return await deliverOwnerSealedJournalKeys(
      forkSpawnId,
      entries,
      target,
      deviceKeys,
      rootPEM,
      now,
      onProgress,
      undefined,
    );
  } catch (e: unknown) {
    if (e instanceof ForkError) throw e;
    const msg = e instanceof Error ? e.message : String(e);
    throw new ForkError(`Fork ${forkSpawnId} journal key delivery is pending: ${msg}`, "delivery", forkSpawnId);
  }
}
