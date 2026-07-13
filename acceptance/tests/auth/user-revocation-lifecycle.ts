export async function cleanupSpawnFailures(
  spawnIds: readonly string[],
  deleteSpawn: (spawnId: string) => Promise<unknown>,
): Promise<unknown[]> {
  const results = await Promise.allSettled(
    spawnIds.map((spawnId) => Promise.resolve().then(() => deleteSpawn(spawnId))),
  );
  return results.flatMap((result) => result.status === "rejected" ? [result.reason] : []);
}

export const NO_LIFECYCLE_FAILURE = Symbol("no lifecycle failure");

interface LifecycleFailure {
  readonly reason: unknown;
}

export type LifecycleFailureState = typeof NO_LIFECYCLE_FAILURE | LifecycleFailure;

export function lifecycleFailure(reason: unknown): LifecycleFailureState {
  return { reason };
}

export function aggregateLifecycleFailures(
  primaryError: LifecycleFailureState,
  restorationError: LifecycleFailureState,
  cleanupErrors: readonly unknown[],
): AggregateError | undefined {
  const errors: unknown[] = [];
  if (primaryError !== NO_LIFECYCLE_FAILURE) errors.push(primaryError.reason);
  if (restorationError !== NO_LIFECYCLE_FAILURE) errors.push(restorationError.reason);
  errors.push(...cleanupErrors);
  if (errors.length === 0) return undefined;
  return new AggregateError(errors, "revocation lifecycle failed");
}
