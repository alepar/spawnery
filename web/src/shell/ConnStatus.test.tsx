import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { ConnStatus } from "./ConnStatus";

describe("ConnStatus", () => {
  it("renders nothing when conn is null", () => {
    render(<ConnStatus conn={null} />);
    expect(screen.queryByTestId("status")).toBeNull();
  });

  it.each([
    ["waiting", "waiting", "bg-zinc-400"],
    ["connecting", "connecting…", "bg-zinc-400"],
    ["slow", "connecting…", "bg-amber-500"],
    ["connected", "connected", "bg-green-500"],
    ["error", "error", "bg-red-500"],
    ["reconnecting", "reconnecting…", "bg-yellow-400"],
    ["disconnected", "disconnected", "bg-red-500"],
  ] as const)("renders %s as a bare dot with the label in title/aria-label + dot color", (state, label, dotClass) => {
    render(<ConnStatus conn={state} />);
    const el = screen.getByTestId("status");
    expect(el.getAttribute("data-status")).toBe(state);
    // The label is exposed via title/aria-label (hover tooltip), not as inline text.
    expect(el.getAttribute("title")).toBe(label);
    expect(el.getAttribute("aria-label")).toBe(label);
    expect(el.textContent).toBe("");
    expect(el.querySelector("span")?.className).toContain(dotClass);
  });
});
