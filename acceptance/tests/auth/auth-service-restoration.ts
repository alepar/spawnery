interface AuthServiceRestoration<T> {
  start(): Promise<void>;
  serviceState(): Promise<string>;
  freshSession(): Promise<T>;
  wait(): Promise<void>;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function restoreAuthService<T>(
  operations: AuthServiceRestoration<T>,
  attempts = 60,
): Promise<T> {
  await operations.start();
  let failure = "health probe did not run";
  for (let attempt = 0; attempt < attempts; attempt++) {
    try {
      const state = await operations.serviceState();
      if (state !== "active") {
        failure = `systemd state ${state}`;
      } else {
        try {
          return await operations.freshSession();
        } catch (error) {
          failure = `fresh session health probe failed: ${errorMessage(error)}`;
        }
      }
    } catch (error) {
      failure = `systemd health probe failed: ${errorMessage(error)}`;
    }
    if (attempt + 1 < attempts) await operations.wait();
  }
  throw new Error(`auth service restoration failed: ${failure}`);
}
