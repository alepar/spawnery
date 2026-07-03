/**
 * Two-layer cleanup (design §Isolation, cleanup): an in-process teardown sweeper removes the
 * current run's namespace even on test failure; a pre-run sweep removes stale acc-* artifacts
 * older than a TTL (covers runs whose process was SIGKILLed/OOMed/CI-timed-out, which the
 * in-process sweeper misses).
 */

import { isAccArtifact, runIdOf, runTimestamp } from "./namespace";
import type { ApiDriver, SpawnSummary } from "../drivers/api";

/**
 * selectStale (pure): given a list of spawn summaries, picks the ones this suite should sweep.
 * - runId given (teardown mode): return exactly that run's acc-* rows, regardless of age.
 * - runId omitted (pre-run mode): return acc-* rows older than `now - ttlMs`, falling back to
 *   createdAt when the name has no parseable embedded timestamp.
 */
export function selectStale(summaries: SpawnSummary[], now: number, ttlMs: number, runId?: string): SpawnSummary[] {
  const accRows = summaries.filter((s) => isAccArtifact(s.name));
  if (runId !== undefined) {
    return accRows.filter((s) => runIdOf(s.name) === runId);
  }
  return accRows.filter((s) => {
    const embedded = runIdOf(s.name);
    const ts = embedded !== null ? runTimestamp(embedded) : null;
    const age = ts !== null ? ts : Number(s.createdAt) * 1000;
    return now - age > ttlMs;
  });
}

/** preRunSweep best-effort deletes stale acc-* spawns; logs (doesn't fail the run) on a single delete error. */
export async function preRunSweep(api: ApiDriver, ttlMs: number, now: number = Date.now()): Promise<void> {
  const summaries = await api.listSpawns();
  const stale = selectStale(summaries, now, ttlMs);
  for (const s of stale) {
    try {
      await api.deleteSpawn(s.spawnId);
    } catch (e) {
      console.warn(`preRunSweep: failed to delete stale spawn ${s.spawnId} (${s.name}): ${(e as Error).message}`);
    }
  }
}

/** teardownSweep deletes every spawn belonging to this run's namespace, even after a test failure. */
export async function teardownSweep(api: ApiDriver, runId: string): Promise<void> {
  const summaries = await api.listSpawns();
  const mine = selectStale(summaries, Date.now(), 0, runId);
  for (const s of mine) {
    try {
      await api.deleteSpawn(s.spawnId);
    } catch (e) {
      console.warn(`teardownSweep: failed to delete spawn ${s.spawnId} (${s.name}): ${(e as Error).message}`);
    }
  }
}
