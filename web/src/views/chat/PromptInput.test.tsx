import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { PromptInput } from "./PromptInput";
import type { Command } from "@/acp/frames";

const CMDS: Command[] = [
  { name: "init", description: "guided setup", inputHint: "$ARGUMENTS" },
  { name: "review", description: "review changes" },
  { name: "compact", description: "compact the thread" },
];

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

  it("shows the command menu when typing `/` and lists every command", async () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} commands={CMDS} />);
    await userEvent.type(screen.getByTestId("prompt-input"), "/");
    expect(screen.getByTestId("command-menu")).toBeTruthy();
    const opts = screen.getAllByTestId("command-option");
    expect(opts.map((o) => o.textContent)).toEqual([
      "/initguided setup",
      "/reviewreview changes",
      "/compactcompact the thread",
    ]);
  });

  it("filters the menu by the typed prefix", async () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} commands={CMDS} />);
    await userEvent.type(screen.getByTestId("prompt-input"), "/re");
    const opts = screen.getAllByTestId("command-option");
    expect(opts).toHaveLength(1);
    expect(opts[0].textContent).toContain("/review");
  });

  it("inserts the command name on click and closes the menu", async () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} commands={CMDS} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "/re");
    await userEvent.click(screen.getByTestId("command-option"));
    expect(box.value).toBe("/review ");
    expect(screen.queryByTestId("command-menu")).toBeNull();
  });

  it("Enter accepts the highlighted command (instead of sending) while the menu is open", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} commands={CMDS} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "/comp");
    await userEvent.keyboard("{Enter}");
    expect(onSend).not.toHaveBeenCalled();
    expect(box.value).toBe("/compact ");
  });

  it("shows nothing when no commands are available (graceful absence)", async () => {
    const onSend = vi.fn();
    render(<PromptInput disabled={false} onSend={onSend} commands={[]} />);
    const box = screen.getByTestId("prompt-input") as HTMLTextAreaElement;
    await userEvent.type(box, "/");
    expect(screen.queryByTestId("command-menu")).toBeNull();
    // With no menu, `/help` is just ordinary text and Enter sends it verbatim.
    await userEvent.type(box, "help");
    await userEvent.keyboard("{Enter}");
    expect(onSend).toHaveBeenCalledWith("/help");
  });

  it("closes the menu once a space follows the command token", async () => {
    render(<PromptInput disabled={false} onSend={vi.fn()} commands={CMDS} />);
    const box = screen.getByTestId("prompt-input");
    await userEvent.type(box, "/review");
    expect(screen.getByTestId("command-menu")).toBeTruthy();
    await userEvent.type(box, " arg");
    expect(screen.queryByTestId("command-menu")).toBeNull();
  });
});
