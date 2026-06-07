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

  it("expands to render a file diff (path + removed/added lines)", async () => {
    const user = userEvent.setup();
    render(
      <ToolCallChip
        title="edit"
        status="completed"
        diff={{ path: "a.go", oldText: "foo\nbaz", newText: "bar\nbaz" }}
      />,
    );
    const toggle = screen.getByTestId("tool-toggle");
    expect(screen.queryByTestId("tool-diff")).toBeNull();

    await user.click(toggle);

    expect(screen.getByTestId("tool-diff-path")).toHaveTextContent("a.go");
    const removed = screen.getAllByTestId("diff-removed").map((n) => n.textContent);
    const added = screen.getAllByTestId("diff-added").map((n) => n.textContent);
    expect(removed).toEqual(["- foo", "- baz"]);
    expect(added).toEqual(["+ bar", "+ baz"]);
  });

  it("renders a diff for a new file (empty oldText -> only added lines)", async () => {
    const user = userEvent.setup();
    render(<ToolCallChip title="write" status="completed" diff={{ path: "new.go", newText: "package x" }} />);
    await user.click(screen.getByTestId("tool-toggle"));
    expect(screen.queryAllByTestId("diff-removed")).toHaveLength(0);
    expect(screen.getByTestId("diff-added")).toHaveTextContent("+ package x");
  });

  it("does not expand for an empty diff", () => {
    render(<ToolCallChip title="edit" status="completed" diff={{}} />);
    expect(screen.queryByTestId("tool-toggle")).toBeNull();
  });
});
