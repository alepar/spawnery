/**
 * wait.ts: polls the apiDriver oracle for a spawn to reach a wanted status — used after a
 * suspend/resume/fork mutation, where journaled operations (blob-store backed) can take a moment.
 * A black-box suite can't probe the target for Garage directly, so a timeout's error carries a
 * diagnostic hint pointing at the likely-missing dependency (design §Risks).
 */

import type { AcceptanceClient, SpawnSummary } from "../drivers/oracle";
import type { SpawnStatus } from "../drivers/types";

export type StatusOracle = Pick<AcceptanceClient, "findSpawn">;

export interface WaitOpts {
  timeoutMs?: number;
  pollMs?: number;
  timeoutHint?: string;
}

const TERMINAL_STATUSES: SpawnStatus[] = ["ERROR", "DELETED"];

export const GARAGE_HINT =
  "suspend/resume/fork require a blob store (Garage) on the target — verify the target has one (design Phase 3).";

/**
 * waitForStatus: polls oracle.findSpawn(id) until .status === want, returning the SpawnSummary.
 * Throws immediately if a terminal status (ERROR/DELETED) is observed while waiting for a
 * different, non-terminal target — that will never resolve on its own. Throws on timeout with a
 * message naming the wanted status, the last-seen status, and (if given) a diagnostic hint.
 */
export async function waitForStatus(oracle: StatusOracle, id: string, want: SpawnStatus, opts: WaitOpts = {}): Promise<SpawnSummary> {
  const timeoutMs = opts.timeoutMs ?? 60_000;
  const pollMs = opts.pollMs ?? 1000;
  const deadline = Date.now() + timeoutMs;
  let last: SpawnSummary | undefined;
  for (;;) {
    last = await oracle.findSpawn(id);
    const status = last?.status;
    if (status === want) return last!;
    if (status !== undefined && status !== want && TERMINAL_STATUSES.includes(status)) {
      throw new Error(`waitForStatus: spawn ${id} reached terminal status ${status} while waiting for ${want}`);
    }
    if (Date.now() > deadline) {
      const hint = opts.timeoutHint ? ` ${opts.timeoutHint}` : "";
      throw new Error(`waitForStatus: spawn ${id} did not reach ${want} within ${timeoutMs}ms (last ${status ?? "unknown"}).${hint}`);
    }
    await new Promise((resolve) => setTimeout(resolve, pollMs));
  }
}
