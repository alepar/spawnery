import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, beforeEach, vi } from "vitest";

// The Terminal tab mounts TermPreview, which constructs an xterm Terminal —
// mock it (and addon-fit) like TerminalView.test.tsx does so it works in jsdom.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
(document as any).fonts = { load: vi.fn().mockResolvedValue([]) };

vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn(() => ({
    loadAddon: vi.fn(),
    open: vi.fn(),
    write: vi.fn(),
    refresh: vi.fn(),
    dispose: vi.fn(),
    options: {},
    rows: 24,
    cols: 80,
  })),
}));
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn(() => ({ fit: vi.fn(), dispose: vi.fn() })),
}));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

import { SettingsView } from "./SettingsView";
import { TermSettingsProvider } from "@/term/settings";

function renderView() {
  return render(
    <TermSettingsProvider>
      <SettingsView />
    </TermSettingsProvider>,
  );
}

describe("SettingsView", () => {
  beforeEach(() => { document.documentElement.className = ""; localStorage.clear(); });

  it("renders with the settings root and General tab active by default", () => {
    renderView();
    expect(screen.getByTestId("settings")).toBeTruthy();
    expect(screen.getByTestId("settings-tab-general")).toBeTruthy();
    expect(screen.getByTestId("settings-tab-terminal")).toBeTruthy();
    // General tab shows the theme toggle; Terminal controls are not mounted.
    expect(screen.getByTestId("theme-toggle")).toBeTruthy();
    expect(screen.queryByTestId("term-follow")).toBeNull();
  });

  it("toggling the switch flips the .dark class and persists", async () => {
    renderView();
    const toggle = screen.getByTestId("theme-toggle");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
    await userEvent.click(toggle);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(localStorage.getItem("theme")).toBe("dark");
  });

  it("clicking the Terminal tab shows the Terminal controls", async () => {
    renderView();
    await userEvent.click(screen.getByTestId("settings-tab-terminal"));
    expect(screen.getByTestId("term-follow")).toBeTruthy();
    expect(screen.queryByTestId("theme-toggle")).toBeNull();
  });
});
