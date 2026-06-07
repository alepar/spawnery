import type { PlanEntry } from "@/acp/frames";
import type { Item } from "@/views/chat/types";

// upsertPlan reconciles a `plan` frame into the items list with REPLACE-IN-PLACE semantics: the agent
// re-sends its WHOLE evolving plan each time, so a new plan supersedes the prior one rather than
// stacking. There is at most ONE plan item in the transcript; later plan frames swap its entries in
// place (same Item id / position), and the checklist re-renders as entries advance pending ->
// in_progress -> completed.
//
// Graceful absence: when no plan ever arrives, no plan item is created and nothing renders. An empty
// plan frame before any plan has been shown is a no-op (no empty checklist); an empty plan frame AFTER
// a plan exists clears it in place (the checklist component renders nothing for zero entries).
// makeId mints the plan item's stable Item id on first creation. Pure: never mutates `items`.
export function upsertPlan(items: Item[], entries: PlanEntry[], makeId: () => number): Item[] {
  const idx = items.findIndex((it) => it.kind === "plan");
  if (idx >= 0) {
    const merged: Item = { ...(items[idx] as Extract<Item, { kind: "plan" }>), entries };
    return [...items.slice(0, idx), merged, ...items.slice(idx + 1)];
  }
  if (entries.length === 0) return items; // no plan yet + nothing to show -> render nothing
  return [...items, { kind: "plan", entries, id: makeId() }];
}
