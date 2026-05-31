import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { Sidebar } from "./Sidebar";

describe("Sidebar", () => {
  it("renders the three nav items and reports selection", async () => {
    const onSelect = vi.fn();
    render(<Sidebar view="chat" onSelect={onSelect} />);
    expect(screen.getByTestId("nav-chat")).toBeTruthy();
    expect(screen.getByTestId("nav-market")).toBeTruthy();
    expect(screen.getByTestId("nav-settings")).toBeTruthy();
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(onSelect).toHaveBeenCalledWith("settings");
  });
});
