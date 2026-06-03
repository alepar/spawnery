import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { ConnStatus } from "./ConnStatus";

describe("ConnStatus", () => {
  it("renders nothing when conn is null", () => {
    render(<ConnStatus conn={null} />);
    expect(screen.queryByTestId("status")).toBeNull();
  });

  it.each([
    ["connecting", "connecting…", "bg-zinc-400"],
    ["slow", "connecting…", "bg-amber-500"],
    ["connected", "connected", "bg-green-500"],
    ["error", "error", "bg-red-500"],
  ] as const)("renders %s with its label + dot color", (state, label, dotClass) => {
    render(<ConnStatus conn={state} />);
    const el = screen.getByTestId("status");
    expect(el.getAttribute("data-status")).toBe(state);
    expect(el.textContent).toContain(label);
    expect(el.querySelector("span")?.className).toContain(dotClass);
  });
});
