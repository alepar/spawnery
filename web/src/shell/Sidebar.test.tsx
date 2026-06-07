import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { Sidebar } from "./Sidebar";
import type { SpawnView } from "@/api/spawnlet";

const spawns: SpawnView[] = [
  { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "" },
  { spawnId: "b", name: "Zork 2", appId: "spawnery/zork", status: "suspended", mode: "" },
  { spawnId: "c", name: "Starting One", appId: "spawnery/wiki", status: "starting", mode: "" },
];
const noopActions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onStop: vi.fn() };

describe("Sidebar", () => {
  it("renders nav (templates+settings, no chat tab)", () => {
    render(<Sidebar view="templates" onSelect={vi.fn()} />);
    expect(screen.getByTestId("nav-templates")).toBeTruthy();
    expect(screen.getByTestId("nav-settings")).toBeTruthy();
    expect(screen.queryByTestId("nav-chat")).toBeNull();
  });

  it("shows the empty placeholder with no spawns", () => {
    render(<Sidebar view="templates" onSelect={vi.fn()} spawns={[]} />);
    expect(screen.getByText("— none yet —")).toBeTruthy();
  });

  it("lists spawns with name headline, app-id subline, and a status dot", () => {
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={noopActions} />);
    expect(screen.getByTestId("spawn-name-a").textContent).toContain("Wiki");
    expect(screen.getByTestId("spawn-row-a").textContent).toContain("spawnery/wiki");
    expect(screen.getByTestId("spawn-dot-a").getAttribute("data-status")).toBe("active");
    expect(screen.getByTestId("spawn-dot-b").getAttribute("data-status")).toBe("suspended");
  });

  it("selects a spawn on row click", async () => {
    const actions = { ...noopActions, onSelectSpawn: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-select-b"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("b");
  });

  it("kebab → Suspend for an active spawn, Resume for a suspended spawn", async () => {
    const actions = { ...noopActions, onSuspend: vi.fn(), onResume: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-suspend-a"));
    expect(actions.onSuspend).toHaveBeenCalledWith("a");
    await userEvent.click(screen.getByTestId("spawn-kebab-b"));
    await userEvent.click(screen.getByTestId("spawn-resume-b"));
    expect(actions.onResume).toHaveBeenCalledWith("b");
  });

  it("kebab → Stop asks for confirm, then calls onStop", async () => {
    const actions = { ...noopActions, onStop: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-stop-a"));
    expect(actions.onStop).not.toHaveBeenCalled(); // first click = arm confirm
    await userEvent.click(screen.getByTestId("spawn-stop-confirm-a"));
    expect(actions.onStop).toHaveBeenCalledWith("a");
  });

  it("renders a yellow pulsing dot for a starting spawn", () => {
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={noopActions} />);
    const dot = screen.getByTestId("spawn-dot-c");
    expect(dot.getAttribute("data-status")).toBe("starting");
    expect(dot.className).toContain("bg-yellow-400");
  });

  it("double-click name → inline edit → Enter renames", async () => {
    const actions = { ...noopActions, onRename: vi.fn() };
    render(<Sidebar view="chat" onSelect={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.dblClick(screen.getByTestId("spawn-name-a"));
    const input = screen.getByTestId("spawn-name-input-a");
    await userEvent.clear(input);
    await userEvent.type(input, "Renamed{Enter}");
    expect(actions.onRename).toHaveBeenCalledWith("a", "Renamed");
  });
});
