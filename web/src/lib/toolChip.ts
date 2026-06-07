import type { Frame } from "@/acp/frames";
import type { Item } from "@/views/chat/types";

type ToolFrame = Extract<Frame, { kind: "tool" }>;
type ToolItem = Extract<Item, { kind: "tool" }>;

// upsertTool reconciles a tool_call / tool_call_update frame into the items list, keyed by toolId:
// the first frame for a toolId appends a chip; later frames merge status/content/raw in place (so one
// tool = one chip). Merges use `patch.x ?? prev.x`, so a status-only update keeps prior title/content.
// makeId mints the chip's stable Item id on creation. Pure: never mutates `items`.
export function upsertTool(items: Item[], f: ToolFrame, makeId: () => number): Item[] {
  const idx = f.toolId ? items.findIndex((it) => it.kind === "tool" && it.toolId === f.toolId) : -1;
  const patch = {
    toolId: f.toolId,
    title: f.title,
    status: f.status,
    content: f.tool?.content,
    rawInput: f.tool?.rawInput,
    rawOutput: f.tool?.rawOutput,
  };
  if (idx >= 0) {
    const prev = items[idx] as ToolItem;
    const merged: Item = {
      ...prev,
      title: patch.title ?? prev.title,
      status: patch.status ?? prev.status,
      content: patch.content ?? prev.content,
      rawInput: patch.rawInput ?? prev.rawInput,
      rawOutput: patch.rawOutput ?? prev.rawOutput,
    };
    return [...items.slice(0, idx), merged, ...items.slice(idx + 1)];
  }
  return [...items, { ...patch, kind: "tool", title: patch.title ?? "tool", id: makeId() }];
}
