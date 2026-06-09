import type { Item } from "@/views/chat/types";
import type { ErrorInfo, Usage } from "@/acp/frames";

// usageBadge returns a short per-turn token/cost label for a turn that reported usage (cat D), or null
// when usage is absent or carries nothing meaningful (UNSTABLE/guarded — a non-reporting agent like goose
// shows no badge). The token figure is input+output (preferring the total the agent reported); cost is
// only appended when actually priced (a pointer/optional on the wire) so we never render a misleading
// $0.00.
export function usageBadge(usage?: Usage): string | null {
  if (!usage) return null;
  const tokens = usage.total ?? (usage.input ?? 0) + (usage.output ?? 0);
  const hasCost = typeof usage.cost === "number" && usage.cost > 0;
  if (tokens <= 0 && !hasCost) return null; // nothing meaningful to show
  const parts: string[] = [];
  if (tokens > 0) parts.push(`${formatTokens(tokens)} tokens`);
  if (hasCost) parts.push(formatCost(usage.cost as number));
  return parts.join(" · ");
}

// formatTokens renders a token count compactly: <1000 verbatim, otherwise as "12.3k" (one decimal).
function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  return `${(n / 1000).toFixed(1)}k`;
}

// formatCost renders a USD cost. Small costs keep more precision so a few-cent turn isn't shown as $0.00.
function formatCost(cost: number): string {
  const decimals = cost < 0.1 ? 4 : 2;
  return `$${cost.toFixed(decimals)}`;
}

// turnEndLabel returns a short human indicator for a turn that ended for a NON-normal reason (cat G),
// or null for a clean end_turn (nothing to show). A structured error takes precedence and shows its
// message; otherwise the StopReason is mapped to a label. Unknown reasons fall through to null.
export function turnEndLabel(turn: { reason?: string; error?: ErrorInfo }): string | null {
  if (turn.error?.message) return `error: ${turn.error.message}`;
  switch (turn.reason) {
    case "max_tokens": return "stopped: max tokens";
    case "max_turn_requests": return "stopped: max requests";
    case "refusal": return "refused";
    case "cancelled": return "cancelled";
    default: return null;
  }
}

// Mirror of internal/node/pump.go's maxQueued. The input box stops sending once this many prompts
// are queued; the pump also drops over-cap as defence in depth.
export const MAX_QUEUED = 50;

// reconcilePending returns items with pending flags adjusted so that exactly `queued` of the most
// recent pending user items stay pending (FIFO drain: oldest pending clears first). It does not
// mutate the input.
export function reconcilePending(items: Item[], queued: number): Item[] {
  const pendingIdx: number[] = [];
  items.forEach((it, i) => {
    if (it.kind === "user" && it.pending) pendingIdx.push(i);
  });
  const clearCount = Math.max(0, pendingIdx.length - queued);
  if (clearCount === 0) return items;
  const clear = new Set(pendingIdx.slice(0, clearCount)); // oldest first
  return items.map((it, i) =>
    clear.has(i) && it.kind === "user" ? { ...it, pending: false } : it,
  );
}
