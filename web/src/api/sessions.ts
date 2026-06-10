import { unary } from "./connect";

export type Transport = "acp" | "mosh";

export interface SessionDescriptor {
  sessionId: string;
  transport: Transport;
  runnable: string;
  status: string;   // "starting" | "active" | "closing" | "closed" | "error"
  pinned: boolean;
}

// runnable/spawn `mode` ("acp"|"tmux") -> session transport ("acp"|"mosh").
export function transportFromMode(mode: string): Transport {
  return mode === "tmux" ? "mosh" : "acp";
}
export function transportToProto(t: Transport): string {
  return t === "mosh" ? "SESSION_TRANSPORT_MOSH" : "SESSION_TRANSPORT_ACP";
}
export function transportFromProto(s: string | undefined): Transport {
  return s === "SESSION_TRANSPORT_MOSH" ? "mosh" : "acp";
}

export async function listSessions(spawnId: string): Promise<SessionDescriptor[]> {
  const r = await unary<{ sessions?: Array<{ sessionId?: string; transport?: string; runnable?: string; status?: string; pinned?: boolean }> }>(
    "ListSessions", { spawnId },
  );
  return (r.sessions ?? []).map((s) => ({
    sessionId: s.sessionId ?? "",
    transport: transportFromProto(s.transport),
    runnable: s.runnable ?? "",
    status: s.status ?? "",
    pinned: !!s.pinned,
  }));
}

export async function createSession(spawnId: string, transport: Transport, runnable: string): Promise<void> {
  await unary<Record<string, never>>("CreateSession", { spawnId, transport: transportToProto(transport), runnable });
}

export async function closeSession(spawnId: string, sessionId: string): Promise<void> {
  await unary<Record<string, never>>("CloseSession", { spawnId, sessionId });
}
