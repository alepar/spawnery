import { unary } from "./connect";
export { DEV_TOKEN } from "./connect";

export type SpawnStatus =
  | "starting" | "active" | "suspending" | "suspended" | "unreachable" | "error" | "unknown";

export interface SpawnView {
  spawnId: string;
  name: string;
  appId: string;
  status: SpawnStatus;
  mode: string;
  model: string;
  modelApplied: boolean;
}

// statusFromProto maps the Connect-JSON enum NAME (e.g. "SPAWN_STATUS_ACTIVE") to a short status.
export function statusFromProto(s: string | undefined): SpawnStatus {
  switch (s) {
    case "SPAWN_STATUS_STARTING": return "starting";
    case "SPAWN_STATUS_ACTIVE": return "active";
    case "SPAWN_STATUS_SUSPENDING": return "suspending";
    case "SPAWN_STATUS_SUSPENDED": return "suspended";
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

export function spawnLifecycleAction(status: SpawnStatus): SpawnLifecycleAction {
  switch (status) {
    case "active": return { kind: "suspend", label: "Suspend" };
    case "suspended": return { kind: "resume", label: "Resume" };
    case "unreachable":
    case "error": return { kind: "recreate", label: "Recreate" };
    case "starting": return { kind: "pending", label: "Starting…" };
    case "suspending": return { kind: "pending", label: "Suspending…" };
    default: return { kind: "pending", label: "Unavailable" }; // unknown
  }
}

export async function createSpawn(appId: string, model: string, image = "", runnableId = ""): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appId, model, image, runnableId });
  return r.spawnId;
}

export async function listSpawns(): Promise<SpawnView[]> {
  const r = await unary<{ spawns?: Array<{ spawnId: string; name?: string; appId?: string; status?: string; mode?: string; model?: string; modelApplied?: boolean }> }>(
    "ListSpawns", {},
  );
  return (r.spawns ?? []).map((s) => ({
    spawnId: s.spawnId,
    name: s.name ?? "",
    appId: s.appId ?? "",
    status: statusFromProto(s.status),
    mode: s.mode ?? "",
    model: s.model ?? "",
    modelApplied: s.modelApplied ?? true,
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
export async function resumeSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("ResumeSpawn", { spawnId });
}
// recreateSpawn re-provisions a fresh container; the recovery path for unreachable/error spawns.
export async function recreateSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("RecreateSpawn", { spawnId });
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
