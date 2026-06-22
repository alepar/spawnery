import { unary } from "./connect";
export { DEV_TOKEN } from "./connect";
import { authEnabled } from "@/auth/session";
import { useSessionStore } from "@/auth/session";
import { pollAndSign, registerPendedOp, clearPendedOp } from "@/auth/intent";

export type SpawnStatus =
  | "starting" | "active" | "suspending" | "suspended" | "resuming" | "unreachable" | "error" | "unknown";

export interface SpawnView {
  spawnId: string;
  name: string;
  appId: string;
  status: SpawnStatus;
  // generation: current live episode generation (cp.proto SpawnStatus.generation). Clients sign it
  // into the session-open intent (see buildSessionOpenSignedIntentB64). uint64 -> JSON string -> bigint.
  // Optional in the type so partial test mocks compile; listSpawns always sets it (defaulting to 0n).
  generation?: bigint;
  mode: string;
  model: string;
  modelApplied: boolean;
  // journalKeyDeliveryPending: spawn is active on target but the browser-side delivery
  // leg was interrupted mid-re-seal (spec §3 "delivery-fail" persistent state, WM3).
  journalKeyDeliveryPending: boolean;
  // transitionPhase: non-empty when status is "suspending" or "resuming"; carries the last
  // phase reported by the node (sp-u53.7.2). Used by the UI to show progress instead of
  // a frozen spinner. Empty for all other statuses.
  transitionPhase: string;
  parentSpawnId?: string;
  forkedAt?: number;
}

// statusFromProto maps the Connect-JSON enum NAME (e.g. "SPAWN_STATUS_ACTIVE") to a short status.
export function statusFromProto(s: string | undefined): SpawnStatus {
  switch (s) {
    case "SPAWN_STATUS_STARTING": return "starting";
    case "SPAWN_STATUS_ACTIVE": return "active";
    case "SPAWN_STATUS_SUSPENDING": return "suspending";
    case "SPAWN_STATUS_SUSPENDED": return "suspended";
    case "SPAWN_STATUS_RESUMING": return "resuming";
    case "SPAWN_STATUS_UNREACHABLE": return "unreachable";
    case "SPAWN_STATUS_ERROR": return "error";
    default: return "unknown";
  }
}

// spawnLifecycleAction maps a status to the single lifecycle menu action the CP will accept.
// "pending" renders disabled (transitional or unknown) so no click can hit a CP precondition failure.
export type SpawnLifecycleAction =
  | { kind: "suspend"; label: string }
  | { kind: "resume"; label: string }
  | { kind: "recreate"; label: string }
  | { kind: "pending"; label: string };

export function spawnLifecycleAction(status: SpawnStatus, transitionPhase?: string, isForkChild = false): SpawnLifecycleAction {
  switch (status) {
    case "active": return { kind: "suspend", label: "Suspend" };
    case "suspended": return { kind: "resume", label: "Resume" };
    case "unreachable":
    case "error": return { kind: "recreate", label: "Recreate" };
    case "starting": return { kind: "pending", label: isForkChild ? "Seeding…" : "Starting…" };
    case "suspending": return { kind: "pending", label: transitionPhase ? `Suspending: ${transitionPhase}` : "Suspending…" };
    case "resuming": return { kind: "pending", label: transitionPhase ? `Resuming: ${transitionPhase}` : "Resuming…" };
    default: return { kind: "pending", label: "Unavailable" }; // unknown
  }
}

export interface CreateMountBinding {
  name: string;
  backendUri: string;
  createIfMissing?: boolean;
}

export async function createSpawn(
  appId: string,
  model: string,
  image = "",
  runnableId = "",
  profileId = "",
  mounts: CreateMountBinding[] = [],
): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appId, model, image, runnableId, profileId, mounts });
  const spawnId = r.spawnId;
  // In auth-enabled mode, kick off the intent signing concurrently (the CP blocks on it).
  if (authEnabled()) {
    const session = useSessionStore.getState();
    const store = session.keyStore;
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(store);
    // appRef is intentionally omitted: the user picks an app *id*, not the immutable app_ref the
    // CP resolves it to (id != ref for catalog/seed apps), so validating against a ref we never
    // supplied would always mismatch and block the spawn. pollAndSign skips the appRef check when
    // it's undefined; the model check still runs and the signed intent uses the CP-resolved appRef.
    const pended = { op: "create-spawn", spawnId, model, mounts };
    registerPendedOp(pended);
    pollAndSign({ spawnId, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }
  return spawnId;
}

export async function listSpawns(): Promise<SpawnView[]> {
  const r = await unary<{ spawns?: Array<{ spawnId: string; name?: string; appId?: string; status?: string; generation?: string | number; mode?: string; model?: string; modelApplied?: boolean; journalKeyDeliveryPending?: boolean; transitionPhase?: string; parentSpawnId?: string; forkedAt?: string | number }> }>(
    "ListSpawns", {},
  );
  return (r.spawns ?? []).map((s) => ({
    spawnId: s.spawnId,
    name: s.name ?? "",
    appId: s.appId ?? "",
    status: statusFromProto(s.status),
    generation: s.generation ? BigInt(s.generation) : 0n,
    mode: s.mode ?? "",
    model: s.model ?? "",
    modelApplied: s.modelApplied ?? true,
    journalKeyDeliveryPending: !!s.journalKeyDeliveryPending,
    transitionPhase: s.transitionPhase ?? "",
    parentSpawnId: s.parentSpawnId ?? "",
    forkedAt: s.forkedAt ? Number(s.forkedAt) : 0,
  }));
}

// SetSpawnModel changes the model an already-running spawn uses (persist + best-effort live push).
// applied=false => saved but the live pod hasn't switched yet (UI shows a "pending" badge).
export async function setSpawnModel(spawnId: string, model: string): Promise<{ model: string; applied: boolean }> {
  return unary<{ model: string; applied: boolean }>("SetSpawnModel", { spawnId, model });
}

export async function renameSpawn(spawnId: string, name: string): Promise<void> {
  await unary<Record<string, never>>("RenameSpawn", { spawnId, name });
}
export async function suspendSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("SuspendSpawn", { spawnId });
}
// ResumeSpawn blocks at the CP awaiting the client's SignedIntent (unlike the async CreateSpawn),
// so pollAndSign MUST run concurrently with the RPC — kicking it off after the await would deadlock
// (CP waits for the intent, client waits for the RPC). Mirrors cmd/spawnctl/resume.go.
export async function resumeSpawn(spawnId: string): Promise<void> {
  if (authEnabled()) {
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    const pended = { op: "resume-spawn", spawnId };
    registerPendedOp(pended);
    pollAndSign({ spawnId, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }
  await unary<Record<string, never>>("ResumeSpawn", { spawnId });
}
// recreateSpawn re-provisions a fresh container; the recovery path for unreachable/error spawns.
export async function recreateSpawn(spawnId: string): Promise<void> {
  // RecreateSpawn blocks at the CP awaiting the SignedIntent (see resumeSpawn): sign concurrently.
  if (authEnabled()) {
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    const pended = { op: "recreate-spawn", spawnId };
    registerPendedOp(pended);
    pollAndSign({ spawnId, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }
  await unary<Record<string, never>>("RecreateSpawn", { spawnId });
}
// migrateSpawn moves a spawn to a new node; requires node-verified intent (DOMAIN_MIGRATE_SPAWN).
export async function migrateSpawn(spawnId: string): Promise<void> {
  // MigrateSpawn blocks at the CP awaiting the SignedIntent (see resumeSpawn): sign concurrently.
  if (authEnabled()) {
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    const pended = { op: "migrate-spawn", spawnId };
    registerPendedOp(pended);
    pollAndSign({ spawnId, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }
  await unary<Record<string, never>>("MigrateSpawn", { spawnId });
}
// UI "Stop" = DeleteSpawn (soft-delete; drops from the list). Legacy stopSpawn kept for non-UI callers.
export async function deleteSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("DeleteSpawn", { spawnId });
}
export async function stopSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("StopSpawn", { spawnId });
}

export interface RunnableView {
  id: string;
  label: string;
  mode: string;
}
export interface AgentImageView {
  image: string;
  runnables: RunnableView[];
}

export async function listAgentImages(): Promise<AgentImageView[]> {
  const r = await unary<{ images?: Array<{ image?: string; runnables?: Array<{ id?: string; label?: string; mode?: string }> }> }>(
    "ListAgentImages", {},
  );
  return (r.images ?? []).map((i) => ({
    image: i.image ?? "",
    runnables: (i.runnables ?? []).map((x) => ({ id: x.id ?? "", label: x.label ?? "", mode: x.mode ?? "" })),
  }));
}
