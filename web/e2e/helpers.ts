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
