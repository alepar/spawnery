import { describe, it, expect } from "vitest";
import { upsertTool } from "./toolChip";
import type { Frame } from "@/acp/frames";
import type { Item } from "@/views/chat/types";

type ToolFrame = Extract<Frame, { kind: "tool" }>;
type ToolItem = Extract<Item, { kind: "tool" }>;

const tool = (f: Omit<ToolFrame, "kind">): ToolFrame => ({ kind: "tool", ...f });

// Deterministic id minter so assertions can check chip identity / count.
function minter(start = 1) {
  let n = start;
  return () => n++;
}

describe("upsertTool", () => {
  it("adds a new chip on tool_call creation (title/status/toolId set)", () => {
    const out = upsertTool([], tool({ toolId: "t1", title: "bash", status: "pending" }), minter());
    expect(out).toHaveLength(1);
    const chip = out[0] as ToolItem;
    expect(chip.kind).toBe("tool");
    expect(chip.toolId).toBe("t1");
    expect(chip.title).toBe("bash");
    expect(chip.status).toBe("pending");
    expect(chip.id).toBe(1);
  });

  it("merges a tool_call_update into the SAME item, preserving prior title and setting content/raw", () => {
    const created = upsertTool([], tool({ toolId: "t1", title: "bash", status: "pending" }), minter());
    const out = upsertTool(
      created,
      tool({
        toolId: "t1",
        status: "completed",
        tool: { content: [{ type: "text", text: "ok" }], rawInput: { cmd: "ls" }, rawOutput: "ok" },
      }),
      minter(99), // must NOT be used: this is a merge, not a create
    );

    expect(out).toHaveLength(1);
    const chip = out[0] as ToolItem;
    expect(chip.id).toBe(1); // same Item, id unchanged
    expect(chip.toolId).toBe("t1");
    expect(chip.title).toBe("bash"); // prior title preserved (update had no title)
    expect(chip.status).toBe("completed");
    expect(chip.content).toEqual([{ type: "text", text: "ok" }]);
    expect(chip.rawInput).toEqual({ cmd: "ls" });
    expect(chip.rawOutput).toBe("ok");
  });

  it("does not wipe previously-set content/title on a status-only update (?? prev behavior)", () => {
    let items = upsertTool(
      [],
      tool({
        toolId: "t1",
        title: "bash",
        status: "in_progress",
        tool: { content: [{ type: "text", text: "partial" }], rawInput: { cmd: "ls" } },
      }),
      minter(),
    );
    // Status-only update: no title, no tool payload.
    items = upsertTool(items, tool({ toolId: "t1", status: "completed" }), minter(99));

    expect(items).toHaveLength(1);
    const chip = items[0] as ToolItem;
    expect(chip.status).toBe("completed"); // status advanced
    expect(chip.title).toBe("bash"); // preserved
    expect(chip.content).toEqual([{ type: "text", text: "partial" }]); // preserved
    expect(chip.rawInput).toEqual({ cmd: "ls" }); // preserved
  });

  it("merges a diff payload into the chip and preserves it on a later status-only update", () => {
    let items = upsertTool([], tool({ toolId: "t1", title: "edit", status: "pending" }), minter());
    items = upsertTool(
      items,
      tool({ toolId: "t1", status: "completed", tool: { diff: { path: "a.go", oldText: "foo", newText: "bar" } } }),
      minter(99),
    );
    let chip = items[0] as ToolItem;
    expect(chip.diff).toEqual({ path: "a.go", oldText: "foo", newText: "bar" });

    // A later status-only update must not wipe the diff.
    items = upsertTool(items, tool({ toolId: "t1", status: "completed" }), minter(99));
    chip = items[0] as ToolItem;
    expect(chip.diff).toEqual({ path: "a.go", oldText: "foo", newText: "bar" });
  });

  it("produces two separate items for two different toolIds", () => {
    const id = minter();
    let items = upsertTool([], tool({ toolId: "t1", title: "bash" }), id);
    items = upsertTool(items, tool({ toolId: "t2", title: "read" }), id);

    expect(items).toHaveLength(2);
    const chips = items as ToolItem[];
    expect(chips.map((c) => c.toolId)).toEqual(["t1", "t2"]);
    expect(chips.map((c) => c.id)).toEqual([1, 2]);
  });
});
