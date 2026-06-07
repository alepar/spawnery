import { render, screen, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { ModeSelector } from "./ModeSelector";
import type { ModePayload } from "@/acp/frames";

const modes: ModePayload = {
  current: "build",
  available: [
    { id: "build", name: "Build" },
    { id: "plan", name: "Plan" },
  ],
};

describe("ModeSelector", () => {
  it("renders the available modes with the current one selected", () => {
    render(<ModeSelector mode={modes} onSetMode={() => {}} />);
    const select = screen.getByLabelText("Session mode") as HTMLSelectElement;
    expect(select.value).toBe("build");
    expect(screen.getByRole("option", { name: "Build" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Plan" })).toBeInTheDocument();
  });

  it("follows the agent's reported current mode", () => {
    render(<ModeSelector mode={{ ...modes, current: "plan" }} onSetMode={() => {}} />);
    expect((screen.getByLabelText("Session mode") as HTMLSelectElement).value).toBe("plan");
  });

  it("emits set_mode with the chosen mode id on change", () => {
    const onSetMode = vi.fn();
    render(<ModeSelector mode={modes} onSetMode={onSetMode} />);
    fireEvent.change(screen.getByLabelText("Session mode"), { target: { value: "plan" } });
    expect(onSetMode).toHaveBeenCalledWith("plan");
  });

  it("renders nothing when the agent has no modes (graceful absence)", () => {
    const { container } = render(<ModeSelector mode={null} onSetMode={() => {}} />);
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("mode-selector")).toBeNull();
  });

  it("renders nothing when there is only one mode (nothing to switch between)", () => {
    const { container } = render(
      <ModeSelector mode={{ current: "build", available: [{ id: "build", name: "Build" }] }} onSetMode={() => {}} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
