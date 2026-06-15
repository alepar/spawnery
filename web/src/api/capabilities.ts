// Regenerate: distrobox enter --root dev-spawnery -- bash -lc 'cd <wt> && go run ./cmd/agentinstall list-agents --capabilities > web/src/api/capabilities.gen.json'
// (JSON cannot hold comments; this header lives in capabilities.ts only.)
import caps from "./capabilities.gen.json";

export type CapabilityStatus = "supported" | "no-op" | "best-effort";

interface CapEntry { kind: string; agent: string; status: CapabilityStatus }

// Build a two-level Map: agent -> kind -> status, from the flat array export.
const _byAgent = new Map<string, Map<string, CapabilityStatus>>();
for (const { kind, agent, status } of caps as CapEntry[]) {
  let m = _byAgent.get(agent);
  if (!m) { m = new Map(); _byAgent.set(agent, m); }
  m.set(kind, status);
}

/** Canonical agent list (deduplicated, order-preserving from the export). */
export const AGENTS: string[] = [...new Set((caps as CapEntry[]).map((e) => e.agent))];

/** Look up the capability status for a (kind, agent) pair. Defaults to "no-op" if absent. */
export function capabilityFor(kind: string, agent: string): CapabilityStatus {
  return _byAgent.get(agent)?.get(kind) ?? "no-op";
}
