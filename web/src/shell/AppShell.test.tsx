import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { AppShell } from "./AppShell";

describe("AppShell", () => {
  it("renders the chat view by default and switches to marketplace", async () => {
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
    // Default view: chat is shown (prompt input present), marketplace not yet mounted.
    expect(screen.getByTestId("prompt-input")).toBeTruthy();
    expect(screen.queryByTestId("marketplace")).toBeNull();

    // Click the Marketplace nav button (Sidebar uses data-testid="nav-market").
    await userEvent.click(screen.getByTestId("nav-market"));
    expect(screen.getByTestId("marketplace")).toBeTruthy();
  });
});
