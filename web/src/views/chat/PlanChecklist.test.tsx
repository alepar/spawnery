import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { PlanChecklist } from "./PlanChecklist";
import type { PlanEntry } from "@/acp/frames";

const e = (content: string, status: string): PlanEntry => ({ content, status, priority: "medium" });

describe("PlanChecklist", () => {
  it("renders each entry with its content and a status glyph (pending/in_progress/completed)", () => {
    render(<PlanChecklist entries={[e("design", "completed"), e("build", "in_progress"), e("test", "pending")]} />);
    const list = screen.getByTestId("plan-checklist");
    expect(list).toBeInTheDocument();

    const rows = screen.getAllByTestId("plan-entry");
    expect(rows).toHaveLength(3);
    expect(rows.map((r) => r.getAttribute("data-status"))).toEqual(["completed", "in_progress", "pending"]);

    expect(screen.getByText("design")).toBeInTheDocument();
    expect(screen.getByText("build")).toBeInTheDocument();
    expect(screen.getByText("test")).toBeInTheDocument();

    // Status glyphs: ☑ completed, ◐ in_progress, ☐ pending.
    expect(screen.getByText("☑")).toBeInTheDocument();
    expect(screen.getByText("◐")).toBeInTheDocument();
    expect(screen.getByText("☐")).toBeInTheDocument();
  });

  it("renders nothing for an empty plan (graceful absence — no empty panel)", () => {
    const { container } = render(<PlanChecklist entries={[]} />);
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("plan-checklist")).toBeNull();
  });
});
