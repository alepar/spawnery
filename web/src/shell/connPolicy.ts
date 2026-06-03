import type { SpawnStatus } from "@/api/spawnlet";
import type { ConnState } from "./useConnStatus";

export type ConnAction = "open" | "error" | "waiting" | "drop" | "none";

// nextConnAction decides what the poll should do for the ACTIVE spawn, given its ledger status, whether
// a live ws already exists, and the current header conn state. Transition-only: returns "none" when the
// desired state is already reflected, so the poll doesn't reopen the ws or re-set the same state.
//   - status undefined (vanished from the ledger)            -> "drop"  (tear down + clear)
//   - active & no ws                                         -> "open"  (connect the ws)
//   - error/unreachable & not already showing error          -> "error" (red — a dead spawn)
//   - starting & not already waiting                         -> "waiting" (grey pulse)
//   - anything else (suspended/suspending)                   -> "none"
export function nextConnAction(status: SpawnStatus | undefined, hasWs: boolean, conn: ConnState | null): ConnAction {
  if (status === undefined) return "drop";
  if (status === "active") return hasWs ? "none" : "open";
  // unreachable (node gone) is a dead spawn too — show red, not a stuck "waiting".
  if (status === "error" || status === "unreachable") return conn === "error" ? "none" : "error";
  if (status === "starting") return conn === "waiting" ? "none" : "waiting";
  return "none";
}
