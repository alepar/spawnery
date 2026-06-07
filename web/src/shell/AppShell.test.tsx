import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { AppShell } from "./AppShell";
import type { SpawnView } from "@/api/spawnlet";

const baseProps = {
  conn: "connected" as const,
  items: [],
  turn: { state: "idle" as const, queued: 0 },
  canSend: true,
  onSend: () => {},
  perm: null,
  onSpawnApp: vi.fn(),
};
const spawns: SpawnView[] = [{ spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "" }];
const actions = { onSelectSpawn: vi.fn(), onRename: vi.fn(), onSuspend: vi.fn(), onResume: vi.fn(), onStop: vi.fn() };

describe("AppShell", () => {
  it("renders the templates by default; chat not mounted", async () => {
    render(<AppShell {...baseProps} />);
    expect(screen.getByTestId("templates")).toBeTruthy();
    expect(screen.queryByTestId("prompt-input")).toBeNull();
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(screen.queryByTestId("templates")).toBeNull();
  });

  it("selecting a spawn navigates to chat and calls onSelectSpawn", async () => {
    render(<AppShell {...baseProps} spawns={spawns} activeId="a" actions={actions} />);
    await userEvent.click(screen.getByTestId("spawn-select-a"));
    expect(actions.onSelectSpawn).toHaveBeenCalledWith("a");
    expect(screen.getByTestId("prompt-input")).toBeTruthy();
  });
});
