// Calls the spawnlet's ConnectRPC unary methods via plain fetch (Connect JSON).
// Proto field names are camelCase in Connect JSON. The Vite dev proxy maps
// /spawn.v1.SpawnService/* to the spawnlet, so these are same-origin (no CORS).

async function unary<T>(method: string, body: unknown): Promise<T> {
  const res = await fetch(`/spawn.v1.SpawnService/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as T;
}

export async function createSpawn(appPath: string, model: string): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appPath, model });
  return r.spawnId;
}

export async function stopSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("StopSpawn", { spawnId });
}
