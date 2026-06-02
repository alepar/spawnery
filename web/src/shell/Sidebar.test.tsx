import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { Sidebar } from "./Sidebar";

describe("Sidebar", () => {
  it("renders the market + settings nav items (no chat tab) and reports selection", async () => {
    const onSelect = vi.fn();
    render(<Sidebar view="market" onSelect={onSelect} />);
    expect(screen.getByTestId("nav-market")).toBeTruthy();
    expect(screen.getByTestId("nav-settings")).toBeTruthy();
    expect(screen.queryByTestId("nav-chat")).toBeNull();
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(onSelect).toHaveBeenCalledWith("settings");
  });

  it("shows an active spawn under Spawns and opens its chat on click", async () => {
    const onSelect = vi.fn();
    render(<Sidebar view="market" onSelect={onSelect} activeSpawn={{ label: "spawnery/secret-app" }} />);
    const spawn = screen.getByTestId("nav-spawn");
    expect(spawn).toBeTruthy();
    expect(spawn.textContent).toContain("spawnery/secret-app");
    await userEvent.click(spawn);
    expect(onSelect).toHaveBeenCalledWith("chat");
  });

  it("shows the empty placeholder when there is no active spawn", () => {
    const onSelect = vi.fn();
    render(<Sidebar view="market" onSelect={onSelect} />);
    expect(screen.queryByTestId("nav-spawn")).toBeNull();
    expect(screen.getByText("— none yet —")).toBeTruthy();
  });
});
