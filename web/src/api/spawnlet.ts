import { unary } from "./connect";
export { DEV_TOKEN } from "./connect";

export async function createSpawn(appId: string, model: string): Promise<string> {
  const r = await unary<{ spawnId: string }>("CreateSpawn", { appId, model });
  return r.spawnId;
}
export async function stopSpawn(spawnId: string): Promise<void> {
  await unary<Record<string, never>>("StopSpawn", { spawnId });
}
