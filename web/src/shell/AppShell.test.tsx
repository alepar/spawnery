import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { AppShell } from "./AppShell";

describe("AppShell", () => {
  it("renders the marketplace view by default and switches via nav", async () => {
    const onSpawnApp = vi.fn();
    render(
      <AppShell
        status="ready"
        items={[]}
        busy={false}
        onSend={() => {}}
        perm={null}
        onSpawnApp={onSpawnApp}
      />,
    );
    // Default view: marketplace is shown, chat prompt not mounted.
    expect(screen.getByTestId("marketplace")).toBeTruthy();
    expect(screen.queryByTestId("prompt-input")).toBeNull();

    // Click the Settings nav button (Sidebar uses data-testid="nav-settings").
    await userEvent.click(screen.getByTestId("nav-settings"));
    expect(screen.queryByTestId("marketplace")).toBeNull();
  });

  it("opens the chat view from the active spawn item in the sidebar", async () => {
    const onSpawnApp = vi.fn();
    render(
      <AppShell
        status="ready"
        items={[]}
        busy={false}
        onSend={() => {}}
        perm={null}
        onSpawnApp={onSpawnApp}
        activeSpawn={{ label: "spawnery/secret-app" }}
      />,
    );
    await userEvent.click(screen.getByTestId("nav-spawn"));
    expect(screen.getByTestId("prompt-input")).toBeTruthy();
  });
});
