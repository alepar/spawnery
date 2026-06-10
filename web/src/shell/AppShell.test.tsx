import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { AppShell } from "./AppShell";
import type { SpawnView } from "@/api/spawnlet";
import type { Nav } from "@/nav/nav";

// SpawnTabs pulls in xterm + live sockets + the ListSessions poll — stub it to an inert marker that
// records the spawnId it was mounted with (its behavior is covered by SpawnTabs.test.tsx).
vi.mock("@/sessions/SpawnTabs", () => ({
  SpawnTabs: ({ spawnId }: { spawnId: string }) => <div data-testid="spawn-tabs">tabs-{spawnId}</div>,
}));

const baseProps = {
  onSpawnApp: vi.fn(),
};
const spawns: SpawnView[] = [{ spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "", model: "", modelApplied: true }];
const actions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onRecreate: vi.fn(), onStop: vi.fn() };

describe("AppShell", () => {
  it("renders templates for the templates section; chat not mounted", () => {
    render(<AppShell {...baseProps} nav={{ section: "templates" }} navigate={vi.fn()} />);
    expect(screen.getByTestId("templates")).toBeTruthy();
    expect(screen.queryByTestId("spawn-tabs")).toBeNull();
  });

  it("renders the settings pane for the settings section", () => {
    render(<AppShell {...baseProps} nav={{ section: "settings" }} navigate={vi.fn()} />);
    expect(screen.queryByTestId("templates")).toBeNull();
  });

  it("renders the spawn tabs for a spawn section and selecting a spawn calls onSelectSpawn", async () => {
    const nav: Nav = { section: "spawn", spawnId: "a" };
    render(<AppShell {...baseProps} nav={nav} navigate={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    expect(screen.getByTestId("spawn-tabs")).toHaveTextContent("tabs-a");
    await userEvent.click(screen.getByTestId("spawn-select-a"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("a");
  });

  it("clicking nav-settings navigates to the settings section", async () => {
    const navigate = vi.fn();
    render(<AppShell {...baseProps} nav={{ section: "templates" }} navigate={navigate} />);
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(navigate).toHaveBeenCalledWith({ section: "settings" });
  });
});
