import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect } from "vitest";
import { ToolCallChip } from "./ToolCallChip";

describe("ToolCallChip", () => {
  it("renders a plain, non-expandable chip when there is no detail", () => {
    render(<ToolCallChip title="bash" status="pending" />);
    expect(screen.getByText(/bash/)).toBeInTheDocument();
    expect(screen.queryByTestId("tool-toggle")).toBeNull();
  });

  it("expands to show the tool result and raw input/output", async () => {
    const user = userEvent.setup();
    render(
      <ToolCallChip
        title="bash"
        status="completed"
        content={[{ type: "text", text: "file1 file2" }]}
        rawInput={{ command: "ls" }}
        rawOutput="file1 file2"
      />,
    );
    // Collapsed: a toggle exists, detail is not yet shown.
    const toggle = screen.getByTestId("tool-toggle");
    expect(screen.queryByTestId("tool-result")).toBeNull();

    await user.click(toggle);

    expect(screen.getByTestId("tool-result")).toHaveTextContent("file1 file2");
    expect(screen.getByTestId("tool-raw-input")).toHaveTextContent('"command": "ls"');
    expect(screen.getByTestId("tool-raw-output")).toHaveTextContent("file1 file2");
  });
});
