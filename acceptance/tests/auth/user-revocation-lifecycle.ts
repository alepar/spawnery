export async function cleanupSpawnFailures(
  spawnIds: readonly string[],
  deleteSpawn: (spawnId: string) => Promise<unknown>,
): Promise<unknown[]> {
  const results = await Promise.allSettled(
    spawnIds.map((spawnId) => Promise.resolve().then(() => deleteSpawn(spawnId))),
  );
  return results.flatMap((result) => result.status === "rejected" ? [result.reason] : []);
}

export function aggregateLifecycleFailures(
  primaryError: unknown,
  restorationError: unknown,
  cleanupErrors: readonly unknown[],
): AggregateError | undefined {
  const errors = [primaryError, restorationError, ...cleanupErrors].filter(
    (error) => error !== undefined && error !== null,
  );
  if (errors.length === 0) return undefined;
  return new AggregateError(errors, "revocation lifecycle failed");
}
