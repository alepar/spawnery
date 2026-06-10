import { type APIRequestContext } from "@playwright/test";

const HEADERS = {
  "Content-Type": "application/json",
  "Connect-Protocol-Version": "1",
  Authorization: "Bearer dev-token",
};

// clearSpawns deletes all of the dev owner's spawns via the CP (proxied through vite), so every test
// starts from a clean ledger (all tests share owner "alice"). Idempotent.
export async function clearSpawns(request: APIRequestContext) {
  const res = await request.post("http://localhost:5173/cp.v1.SpawnService/ListSpawns", { headers: HEADERS, data: {} });
  const body = await res.json().catch(() => ({}));
  for (const s of body.spawns ?? []) {
    await request.post("http://localhost:5173/cp.v1.SpawnService/DeleteSpawn", { headers: HEADERS, data: { spawnId: s.spawnId } });
  }
}

export interface SessionRow {
  sessionId: string;
  transport: string; // proto enum, e.g. "SESSION_TRANSPORT_ACP" | "SESSION_TRANSPORT_MOSH"
  runnable: string;  // "stub-acp" | "shell" | ...
  status: string;    // proto enum SessionState_*
  pinned: boolean;
}

// listSessions reads CP's mirrored, node-authoritative session roster for a spawn (the same RPC the
// web SpawnTabs polls). Used by the multi-session e2e to assert concurrency + per-session reap.
export async function listSessions(request: APIRequestContext, spawnId: string): Promise<SessionRow[]> {
  const res = await request.post("http://localhost:5173/cp.v1.SpawnService/ListSessions", { headers: HEADERS, data: { spawnId } });
  const body = await res.json().catch(() => ({}));
  return (body.sessions ?? []).map((s: any) => ({
    sessionId: s.sessionId ?? "",
    transport: s.transport ?? "",
    runnable: s.runnable ?? "",
    status: s.status ?? "",
    pinned: !!s.pinned,
  }));
}
