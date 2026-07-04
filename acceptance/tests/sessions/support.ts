/**
 * Phase-2 (sessions) test support: a per-run+per-invocation unique marker for the exec
 * side-effect check, an ACTIVE-status poller against the apiDriver oracle, a usage-badge parser
 * feeding the CostLedger, and precondition readers for the @agent-only env vars — all pure or
 * thin-wrapping helpers, unit-tested in support.test.ts.
 */

import type { ApiDriver } from "../../src/drivers/api";
import type { SpawnId } from "../../src/drivers/types";
import type { SessionPage } from "../../src/drivers/session";
import type { CostLedger } from "../../src/fixtures/budget";
import type { TargetConfig } from "../../src/config/target";

/** slug lowercases and collapses a free-form test name into a marker-safe token. */
function slug(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

let markerSeq = 0;

/**
 * newMarker mints a per-run + per-invocation-unique marker for a "prove this is a FRESH
 * side-effect, not a stale one from a prior run" check (design line 143). runId scopes it to
 * this run; a monotonic in-process sequence (alongside the timestamp) guarantees two calls in
 * the same test process never collide, even within the same millisecond.
 */
export function newMarker(runId: string, testName: string, now: () => number = Date.now): string {
  markerSeq += 1;
  return `ACC-${runId}-${slug(testName)}-${now().toString(36)}-${markerSeq.toString(36)}`;
}

/**
 * parseUsageTokens extracts a token count from a rendered `turn-usage-badge` label (e.g.
 * "12.3k tokens · $0.04" or "500 tokens"), or null when the badge is absent or unparseable — the
 * "no usage reported" case (web/src/lib/turn.ts's usageBadge already guards this on render).
 */
export function parseUsageTokens(badge: string | null): number | null {
  if (!badge) return null;
  const m = /([\d.]+)\s*(k)?\s*tokens/i.exec(badge);
  if (!m) return null;
  const n = parseFloat(m[1]);
  if (!Number.isFinite(n)) return null;
  return Math.round(m[2] ? n * 1000 : n);
}

/**
 * recordAgentCost reads the turn's usage badge (falling back to `estimate` tokens when the agent
 * reported none), feeds it into the ledger, and checks the ledger's cap — throwing
 * BudgetExceededError to abort the run when the per-run token budget or wall-clock cap is blown.
 */
export async function recordAgentCost(page: SessionPage, ledger: CostLedger, estimate: number): Promise<void> {
  const tokens = parseUsageTokens(await page.usageBadgeText());
  ledger.recordTokens(tokens ?? estimate);
  ledger.check();
}

/**
 * waitActiveApi polls `api.findSpawn` (the surface-agnostic oracle — no GetSpawn RPC exists)
 * until the spawn reports ACTIVE, throwing on a terminal ERROR/DELETED status or on timeout
 * (naming the last-seen status either way).
 */
export async function waitActiveApi(
  api: ApiDriver,
  id: SpawnId,
  opts: { timeoutMs?: number; pollMs?: number } = {},
): Promise<void> {
  // Real targets (a runsc pod on a VM) take ~1-2min to reach ACTIVE — far longer than the hermetic
  // stub. Default stays 90s (stub); ACC_SPAWN_ACTIVE_TIMEOUT_MS raises it for real-node targets.
  const timeoutMs =
    opts.timeoutMs ??
    (process.env.ACC_SPAWN_ACTIVE_TIMEOUT_MS ? Number(process.env.ACC_SPAWN_ACTIVE_TIMEOUT_MS) : 90_000);
  const pollMs = opts.pollMs ?? 1500;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const spawn = await api.findSpawn(id);
    const status = spawn?.status;
    if (status === "ACTIVE") return;
    if (status === "ERROR" || status === "DELETED") {
      throw new Error(`waitActiveApi: spawn ${id} reached terminal status ${status} while waiting for ACTIVE`);
    }
    if (Date.now() > deadline) {
      throw new Error(`waitActiveApi: timed out waiting for spawn ${id} to become ACTIVE (last status ${status ?? "not found"})`);
    }
    await new Promise((resolve) => setTimeout(resolve, pollMs));
  }
}

/**
 * agentAppId/agentModel read the @agent-only precondition vars off TargetConfig, THROWING (never
 * skipping, per this project's "fail, don't skip" rule) a clear message naming the missing var
 * when a Phase-2 @agent scenario runs against a target that hasn't set them up.
 */
export function agentAppId(target: TargetConfig): string {
  if (!target.agentAppId) {
    throw new Error(
      "Phase-2 @agent scenarios require ACC_AGENT_APP_ID — a real coding-agent app registered on the target (see acceptance/README.md)",
    );
  }
  return target.agentAppId;
}

export function agentModel(target: TargetConfig): string {
  if (!target.agentModel) {
    throw new Error(
      "Phase-2 @agent scenarios require ACC_AGENT_MODEL — a pinned, cheap model for cost control (see acceptance/README.md)",
    );
  }
  return target.agentModel;
}
