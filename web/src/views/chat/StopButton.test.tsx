import { render, screen, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { StopButton } from "./StopButton";

describe("StopButton", () => {
  it("is visible while a turn is busy", () => {
    render(<StopButton busy={true} onCancel={() => {}} />);
    expect(screen.getByTestId("stop-button")).toBeInTheDocument();
  });

  it("is hidden when the turn is idle (nothing to cancel)", () => {
    const { container } = render(<StopButton busy={false} onCancel={() => {}} />);
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("stop-button")).toBeNull();
  });

  it("emits cancel on click", () => {
    const onCancel = vi.fn();
    render(<StopButton busy={true} onCancel={onCancel} />);
    fireEvent.click(screen.getByTestId("stop-button"));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
