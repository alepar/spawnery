import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { PromptInput } from "./PromptInput";

describe("PromptInput", () => {
  it("sends text on Enter then clears the box", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hello");
    await userEvent.keyboard("{Enter}");
    expect(onSend).toHaveBeenCalledWith("hello");
    expect(box.value).toBe("");
  });

  it("does not send whitespace-only input", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    await userEvent.type(screen.getByTestId("prompt-input"), "   ");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
  });

  it("keeps focus on the input after Enter (does not blur)", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hi");
    await userEvent.keyboard("{Enter}");
    expect(document.activeElement).toBe(box);
  });

  it("does not send while busy (disabled), but the box stays focusable", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={true} onSend={onSend} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "hi"); // still typeable (not disabled) — type-ahead
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("hi"); // retained, not cleared
  });
});
