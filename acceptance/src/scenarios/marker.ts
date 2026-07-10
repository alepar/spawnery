/**
 * marker.ts: per-run unique markers written into a spawn's workspace via `spawnctl exec`, used to
 * prove suspend/resume and fork preserve workspace state. assertFreshMarker deliberately checks
 * equality with THIS run's marker value, not mere file existence — a stale marker left over from a
 * prior run would satisfy an existence check but not a freshness one (design §Assertion strategy).
 */

import { execOrThrow, type ExecConfig } from "./exec";

/** newMarker: a value unique to this (runId, spawnId, call) — collisions would mask a real bug. */
export function newMarker(runId: string, spawnId: string): string {
  const time = Date.now().toString(36);
  const rand = Math.random().toString(36).slice(2, 8);
  return `${runId}-${spawnId}-${time}-${rand}`;
}

/** markerPath: the marker file, in the journaled node-local workspace mount (/app/data), namespaced by runId. */
export function markerPath(runId: string): string {
  return `/app/data/.acc-marker-${runId}`;
}

/** writeMarker: writes `marker` to markerPath(runId) inside the spawn's agent container. */
export async function writeMarker(cfg: ExecConfig, spawnId: string, runId: string, marker: string): Promise<void> {
  const path = markerPath(runId);
  await execOrThrow(cfg, spawnId, ["sh", "-c", `printf %s '${marker}' > '${path}'`]);
}

/** readMarker: reads markerPath(runId) back from the spawn's agent container (untrimmed stdout). */
export async function readMarker(cfg: ExecConfig, spawnId: string, runId: string): Promise<string> {
  const { stdout } = await execOrThrow(cfg, spawnId, ["cat", markerPath(runId)]);
  return stdout;
}

/**
 * assertFreshMarker: throws unless `got` (trimmed) equals `expected` — framed explicitly as a
 * stale/lost-state failure, never an "exists" check (design §Assertion strategy).
 */
export function assertFreshMarker(got: string, expected: string): void {
  const trimmed = got.trim();
  if (trimmed !== expected) {
    throw new Error(
      `assertFreshMarker: marker is stale or state was lost — got ${JSON.stringify(trimmed)}, expected ${JSON.stringify(expected)}`,
    );
  }
}
