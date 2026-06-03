import type { Item } from "@/views/chat/types";

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
