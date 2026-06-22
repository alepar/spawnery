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
// ProvisioningPane behavior is covered by ProvisioningPane.test.tsx — stub to an inert marker.
vi.mock("@/sessions/ProvisioningPane", () => ({
  ProvisioningPane: ({ spawn }: { spawn: { status: string } }) => <div data-testid="provisioning-pane" data-state={spawn.status}>pane-{spawn.status}</div>,
}));

const baseProps = {
  onSpawnApp: vi.fn(),
};
const spawns: SpawnView[] = [{ spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "", provisionStep: 0, provisionTotal: 0, provisionStepLabel: "", errorStep: "", errorDetail: "" }];
const startingSpawns: SpawnView[] = [{ spawnId: "b", name: "New", appId: "spawnery/wiki", status: "starting", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "", provisionStep: 2, provisionTotal: 5, provisionStepLabel: "cloning repo", errorStep: "", errorDetail: "" }];
const errorSpawns: SpawnView[] = [{ spawnId: "c", name: "Broken", appId: "spawnery/wiki", status: "error", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "", provisionStep: 0, provisionTotal: 0, provisionStepLabel: "", errorStep: "prepare-mounts", errorDetail: "403 forbidden" }];
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

  it("renders provisioning-pane (not spawn-tabs) for a starting spawn", () => {
    const nav: Nav = { section: "spawn", spawnId: "b" };
    render(<AppShell {...baseProps} nav={nav} navigate={vi.fn()} spawns={startingSpawns} activeId="b" actions={actions} />);
    expect(screen.getByTestId("provisioning-pane")).toBeInTheDocument();
    expect(screen.queryByTestId("spawn-tabs")).toBeNull();
  });

  it("renders provisioning-pane (not spawn-tabs) for an error spawn", () => {
    const nav: Nav = { section: "spawn", spawnId: "c" };
    render(<AppShell {...baseProps} nav={nav} navigate={vi.fn()} spawns={errorSpawns} activeId="c" actions={actions} />);
    expect(screen.getByTestId("provisioning-pane")).toBeInTheDocument();
    expect(screen.queryByTestId("spawn-tabs")).toBeNull();
  });

  it("still renders spawn-tabs for an active spawn (regression guard)", () => {
    const nav: Nav = { section: "spawn", spawnId: "a" };
    render(<AppShell {...baseProps} nav={nav} navigate={vi.fn()} spawns={spawns} activeId="a" actions={actions} />);
    expect(screen.getByTestId("spawn-tabs")).toBeInTheDocument();
    expect(screen.queryByTestId("provisioning-pane")).toBeNull();
  });
});
