import { cpv1 } from "@spawnery/client";

const CAPACITY_DETAIL = "resource_exhausted: no eligible node with capacity";

interface PendingCapacityClient<TRequest, TPending> {
  createSpawn(request: TRequest): Promise<{ spawnId: string }>;
  getPendingIntent(request: { spawnId: string }): Promise<{ ready: boolean; pending?: TPending }>;
  listSpawns(request: Record<string, never>): Promise<{ spawns: Array<{
    spawnId: string;
    status: cpv1.SpawnStatus;
    errorStep?: string;
    errorDetail?: string;
  }> }>;
  deleteSpawn(request: { spawnId: string }): Promise<unknown>;
}

interface CapacityRetryOptions {
  maxAttempts?: number;
  pollAttempts?: number;
  pollMs?: number;
  retryMs?: number;
  sleep?: (ms: number) => Promise<void>;
}

export class PendingSpawnTerminalError extends Error {
  readonly name = "PendingSpawnTerminalError";

  constructor(
    readonly spawnId: string,
    readonly errorStep: string,
    readonly errorDetail: string,
  ) {
    super(`spawn ${spawnId} entered ERROR at ${errorStep || "unknown step"}: ${errorDetail || "no detail"}`);
  }
}

async function waitForPendingOrTerminal<TRequest, TPending>(
  client: PendingCapacityClient<TRequest, TPending>,
  spawnId: string,
  pollAttempts: number,
  pollMs: number,
  sleep: (ms: number) => Promise<void>,
): Promise<TPending> {
  for (let attempt = 0; attempt < pollAttempts; attempt++) {
    const response = await client.getPendingIntent({ spawnId });
    if (response.ready && response.pending) return response.pending;

    const row = (await client.listSpawns({})).spawns.find((spawn) => spawn.spawnId === spawnId);
    if (row?.status === cpv1.SpawnStatus.ERROR) {
      throw new PendingSpawnTerminalError(spawnId, row.errorStep ?? "", row.errorDetail ?? "");
    }
    if (attempt + 1 < pollAttempts) await sleep(pollMs);
  }
  throw new Error(`pending intent did not become ready for ${spawnId}`);
}

/**
 * Bridges only the bounded node-capacity convergence window after a prior test's confirmed CP
 * deletions. It is intentionally not a generic create retry: any other terminal step/detail is
 * returned immediately, and every failed probe row is deleted before retrying or failing.
 */
export async function acquirePendingAfterCapacityConverges<TRequest, TPending>(
  client: PendingCapacityClient<TRequest, TPending>,
  request: TRequest,
  options: CapacityRetryOptions = {},
): Promise<{ created: { spawnId: string }; pending: TPending }> {
  const maxAttempts = options.maxAttempts ?? 30;
  const pollAttempts = options.pollAttempts ?? 120;
  const pollMs = options.pollMs ?? 250;
  const retryMs = options.retryMs ?? 500;
  const sleep = options.sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    const created = await client.createSpawn(request);
    try {
      const pending = await waitForPendingOrTerminal(client, created.spawnId, pollAttempts, pollMs, sleep);
      return { created, pending };
    } catch (error) {
      await client.deleteSpawn({ spawnId: created.spawnId });
      const capacityLag = error instanceof PendingSpawnTerminalError && error.errorDetail === CAPACITY_DETAIL;
      if (!capacityLag) throw error;
      if (attempt === maxAttempts) {
        throw new Error(`node capacity did not converge after ${maxAttempts} attempts: ${error.message}`);
      }
      await sleep(retryMs);
    }
  }
  throw new Error("unreachable capacity retry state");
}
