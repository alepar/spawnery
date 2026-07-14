export interface TemporarySPALifecycleSteps {
  publishTemporary: () => Promise<unknown>;
  runWithTemporary: () => Promise<unknown>;
  restoreOriginal: () => Promise<unknown>;
  removeTemporary: () => Promise<unknown>;
  verifyRestoredHealth: () => Promise<unknown>;
  reloadRestoredPage: () => Promise<unknown>;
}

export async function runTemporarySPALifecycle(
  steps: TemporarySPALifecycleSteps,
): Promise<void> {
  const failures: unknown[] = [];
  // Capture the mandatory recovery sequence before publication, which may mutate state and throw.
  const recoverySteps = [
    steps.restoreOriginal,
    steps.removeTemporary,
    steps.verifyRestoredHealth,
    steps.reloadRestoredPage,
  ];
  try {
    await steps.publishTemporary();
    await steps.runWithTemporary();
  } catch (reason) {
    failures.push(reason);
  }

  for (const cleanup of recoverySteps) {
    try {
      await cleanup();
    } catch (reason) {
      failures.push(reason);
    }
  }

  if (failures.length > 0) {
    throw new AggregateError(failures, "temporary SPA lifecycle failed");
  }
}
