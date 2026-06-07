import type { Item } from "@/views/chat/types";
import type { ErrorInfo } from "@/acp/frames";

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

// Mirror of internal/transcript.MaxQueued. The input box stops sending once this many prompts are
// queued; the broker also drops over-cap as defence in depth.
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
