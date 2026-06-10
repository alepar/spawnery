import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { PermissionModal } from "./PermissionModal";
import type { PermOption } from "@/acp/frames";

const OPTIONS: PermOption[] = [
  { optionId: "allow_once", name: "Allow once", kind: "allow_once" },
  { optionId: "allow_always", name: "Allow always", kind: "allow_always" },
  { optionId: "reject_once", name: "Reject", kind: "reject_once" },
];

describe("PermissionModal", () => {
  it("renders one button per agent option, labeled by name", () => {
    render(<PermissionModal title="run bash" options={OPTIONS} onResolve={() => {}} />);
    expect(screen.getByText("run bash")).toBeInTheDocument();
    for (const o of OPTIONS) {
      expect(screen.getByTestId(`perm-option-${o.optionId}`)).toHaveTextContent(o.name!);
    }
  });

  it("resolves with the picked optionId", async () => {
    const user = userEvent.setup();
    const onResolve = vi.fn();
    render(<PermissionModal title="run bash" options={OPTIONS} onResolve={onResolve} />);
    await user.click(screen.getByTestId("perm-option-allow_always"));
    expect(onResolve).toHaveBeenCalledWith("allow_always");
  });

  it("resolves with a reject optionId when the reject button is clicked", async () => {
    const user = userEvent.setup();
    const onResolve = vi.fn();
    render(<PermissionModal title="run bash" options={OPTIONS} onResolve={onResolve} />);
    await user.click(screen.getByTestId("perm-option-reject_once"));
    expect(onResolve).toHaveBeenCalledWith("reject_once");
  });

  it("resolves with \"\" (auto-deny) when dismissed via Escape", async () => {
    const user = userEvent.setup();
    const onResolve = vi.fn();
    render(<PermissionModal title="run bash" options={OPTIONS} onResolve={onResolve} />);
    await user.keyboard("{Escape}");
    expect(onResolve).toHaveBeenCalledWith("");
  });
});
