import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { Sidebar } from "./Sidebar";
import type { SpawnView } from "@/api/spawnlet";
import type { Nav } from "@/nav/nav";

const spawns: SpawnView[] = [
  { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
  { spawnId: "b", name: "Zork 2", appId: "spawnery/zork", status: "suspended", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
  { spawnId: "c", name: "Starting One", appId: "spawnery/wiki", status: "starting", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
  { spawnId: "d", name: "Reaped", appId: "spawnery/wiki", status: "unreachable", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
  { spawnId: "e", name: "Broken", appId: "spawnery/wiki", status: "error", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
  { spawnId: "f", name: "Winding Down", appId: "spawnery/wiki", status: "suspending", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "" },
];
const noopActions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onRecreate: vi.fn(), onStop: vi.fn() };
const templatesNav: Nav = { section: "templates" };
const spawnNav = (id: string): Nav => ({ section: "spawn", spawnId: id });

describe("Sidebar", () => {
  it("renders nav (templates+settings, no chat tab)", () => {
    render(<Sidebar nav={templatesNav} navigate={vi.fn()} />);
    expect(screen.getByTestId("nav-templates")).toBeTruthy();
    expect(screen.getByTestId("nav-settings")).toBeTruthy();
    expect(screen.queryByTestId("nav-chat")).toBeNull();
  });

  it("clicking nav buttons navigates to the right section", async () => {
    const navigate = vi.fn();
    render(<Sidebar nav={templatesNav} navigate={navigate} />);
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(navigate).toHaveBeenCalledWith({ section: "settings" });
    await userEvent.click(screen.getByTestId("nav-templates"));
    expect(navigate).toHaveBeenCalledWith({ section: "templates" });
  });

  it("Templates button is active across templates/app/my-apps/publish sections", () => {
    const sections: Nav[] = [
      { section: "templates" },
      { section: "app", appId: "spawnery/wiki" },
      { section: "my-apps" },
      { section: "publish" },
    ];
    for (const nav of sections) {
      const { unmount } = render(<Sidebar nav={nav} navigate={vi.fn()} />);
      // the active button renders the "secondary" Button variant, the inactive one "ghost".
      expect(screen.getByTestId("nav-templates").getAttribute("data-variant")).toBe("secondary");
      expect(screen.getByTestId("nav-settings").getAttribute("data-variant")).toBe("ghost");
      unmount();
    }
  });

  it("Settings button is active only for the settings section", () => {
    render(<Sidebar nav={{ section: "settings" }} navigate={vi.fn()} />);
    expect(screen.getByTestId("nav-templates").getAttribute("data-variant")).toBe("ghost");
    expect(screen.getByTestId("nav-settings").getAttribute("data-variant")).toBe("secondary");
  });

  it("shows the empty placeholder with no spawns", () => {
    render(<Sidebar nav={templatesNav} navigate={vi.fn()} spawns={[]} />);
    expect(screen.getByText("— none yet —")).toBeTruthy();
  });

  it("lists spawns with name headline, app-id subline, and a status dot", () => {
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={noopActions} />);
    expect(screen.getByTestId("spawn-name-a").textContent).toContain("Wiki");
    expect(screen.getByTestId("spawn-row-a").textContent).toContain("spawnery/wiki");
    expect(screen.getByTestId("spawn-dot-a").getAttribute("data-status")).toBe("active");
    expect(screen.getByTestId("spawn-dot-b").getAttribute("data-status")).toBe("suspended");
  });

  it.each([
    ["a", "active", "active"],
    ["b", "suspended", "suspended"],
    ["c", "starting", "starting…"],
    ["d", "unreachable", "unreachable"],
    ["e", "error", "error"],
    ["f", "suspending", "suspending…"],
  ] as const)("exposes %s status label via title/aria-label without inline text", (id, status, label) => {
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={noopActions} />);
    const dot = screen.getByTestId(`spawn-dot-${id}`);
    expect(dot.getAttribute("data-status")).toBe(status);
    expect(dot.getAttribute("title")).toBe(label);
    expect(dot.getAttribute("aria-label")).toBe(label);
    expect(dot.textContent).toBe("");
  });

  it("highlights the spawn row matching nav.spawnId", () => {
    render(<Sidebar nav={spawnNav("b")} navigate={vi.fn()} spawns={spawns} actions={noopActions} />);
    expect(screen.getByTestId("spawn-row-b").className).toContain("bg-secondary");
    expect(screen.getByTestId("spawn-row-a").className).not.toContain("bg-secondary");
  });

  it("selects a spawn on row click", async () => {
    const actions = { ...noopActions, onSelectSpawn: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-select-b"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("b");
  });

  it("kebab → Suspend for an active spawn, Resume for a suspended spawn", async () => {
    const actions = { ...noopActions, onSuspend: vi.fn(), onResume: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-suspend-a"));
    expect(actions.onSuspend).toHaveBeenCalledWith("a");
    await userEvent.click(screen.getByTestId("spawn-kebab-b"));
    await userEvent.click(screen.getByTestId("spawn-resume-b"));
    expect(actions.onResume).toHaveBeenCalledWith("b");
  });

  it("kebab → Recreate for an unreachable spawn calls onRecreate", async () => {
    const actions = { ...noopActions, onRecreate: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-d"));
    const item = screen.getByTestId("spawn-recreate-d");
    expect(item.textContent).toBe("Recreate");
    await userEvent.click(item);
    expect(actions.onRecreate).toHaveBeenCalledWith("d");
  });

  it("kebab → Recreate for an error spawn calls onRecreate", async () => {
    const actions = { ...noopActions, onRecreate: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-e"));
    const item = screen.getByTestId("spawn-recreate-e");
    expect(item.textContent).toBe("Recreate");
    await userEvent.click(item);
    expect(actions.onRecreate).toHaveBeenCalledWith("e");
  });

  it("kebab → disabled pending item for a starting spawn; clicking dispatches nothing", async () => {
    const actions = { ...noopActions, onSuspend: vi.fn(), onResume: vi.fn(), onRecreate: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-c"));
    const item = screen.getByTestId("spawn-pending-c");
    expect(item.textContent).toBe("Starting…");
    expect((item as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(item);
    expect(actions.onSuspend).not.toHaveBeenCalled();
    expect(actions.onResume).not.toHaveBeenCalled();
    expect(actions.onRecreate).not.toHaveBeenCalled();
  });

  it("kebab → disabled pending item for a suspending spawn; clicking dispatches nothing", async () => {
    const actions = { ...noopActions, onSuspend: vi.fn(), onResume: vi.fn(), onRecreate: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-f"));
    const item = screen.getByTestId("spawn-pending-f");
    expect(item.textContent).toBe("Suspending…");
    expect((item as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(item);
    expect(actions.onSuspend).not.toHaveBeenCalled();
    expect(actions.onResume).not.toHaveBeenCalled();
    expect(actions.onRecreate).not.toHaveBeenCalled();
  });

  it("kebab → Stop asks for confirm, then calls onStop", async () => {
    const actions = { ...noopActions, onStop: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-kebab-a"));
    await userEvent.click(screen.getByTestId("spawn-stop-a"));
    expect(actions.onStop).not.toHaveBeenCalled(); // first click = arm confirm
    await userEvent.click(screen.getByTestId("spawn-stop-confirm-a"));
    expect(actions.onStop).toHaveBeenCalledWith("a");
  });

  it("renders a yellow pulsing dot for a starting spawn", () => {
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={noopActions} />);
    const dot = screen.getByTestId("spawn-dot-c");
    expect(dot.getAttribute("data-status")).toBe("starting");
    expect(dot.className).toContain("bg-yellow-400");
  });

  it("double-click name → inline edit → Enter renames", async () => {
    const actions = { ...noopActions, onRename: vi.fn() };
    render(<Sidebar nav={spawnNav("a")} navigate={vi.fn()} spawns={spawns} actions={actions} />);
    await userEvent.dblClick(screen.getByTestId("spawn-name-a"));
    const input = screen.getByTestId("spawn-name-input-a");
    await userEvent.clear(input);
    await userEvent.type(input, "Renamed{Enter}");
    expect(actions.onRename).toHaveBeenCalledWith("a", "Renamed");
  });
});
