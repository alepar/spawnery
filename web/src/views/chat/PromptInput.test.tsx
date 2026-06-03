import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { PromptInput } from "./PromptInput";

describe("PromptInput", () => {
  it("renders no Send button (Enter sends)", () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} />);
    expect(screen.queryByTestId("prompt-send")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("sends text on Enter then clears the box", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hello");
    await userEvent.keyboard("{Enter}");
    expect(onSend).toHaveBeenCalledWith("hello");
    expect(box.value).toBe("");
  });

  it("Shift+Enter inserts a newline and does not send", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "line1");
    await userEvent.keyboard("{Shift>}{Enter}{/Shift}");
    await userEvent.type(box, "line2");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("line1\nline2");
  });

  it("does not send whitespace-only input", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    await userEvent.type(screen.getByTestId("prompt-input"), "   ");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
  });

  it("does not send while disabled, but the box stays typeable and retains text", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={true} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hi");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("hi");
  });
});
