import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, beforeEach, vi } from "vitest";

// TermPreview (rendered inside TerminalSettings) constructs an xterm Terminal —
// mock it like TerminalView.test.tsx so it works under jsdom.
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

import { TerminalSettings } from "./TerminalSettings";
import { TermSettingsProvider, loadTermSettings } from "@/term/settings";
import { TERM_THEME_NAMES } from "@/term/themes.gen";

function renderPane() {
  return render(
    <TermSettingsProvider>
      <TerminalSettings />
    </TermSettingsProvider>,
  );
}

describe("TerminalSettings", () => {
  beforeEach(() => { localStorage.clear(); document.documentElement.className = ""; });

  it("with follow on (default) shows the light/dark pair and no fixed select", () => {
    renderPane();
    expect((screen.getByTestId("term-follow") as HTMLButtonElement).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByTestId("term-light")).toBeTruthy();
    expect(screen.getByTestId("term-dark")).toBeTruthy();
    expect(screen.queryByTestId("term-fixed")).toBeNull();
  });

  it("toggling follow off swaps to the single fixed select", async () => {
    renderPane();
    await userEvent.click(screen.getByTestId("term-follow"));
    expect(screen.getByTestId("term-fixed")).toBeTruthy();
    expect(screen.queryByTestId("term-light")).toBeNull();
    expect(screen.queryByTestId("term-dark")).toBeNull();
    expect(loadTermSettings().follow).toBe(false);
  });

  it("changing a scheme select updates the store and the rendered value", async () => {
    renderPane();
    const other = TERM_THEME_NAMES.find((n) => n !== loadTermSettings().dark)!;
    const sel = screen.getByTestId("term-dark") as HTMLSelectElement;
    await userEvent.selectOptions(sel, other);
    expect(sel.value).toBe(other);
    expect(loadTermSettings().dark).toBe(other);
  });

  it("changing the font select updates the store", async () => {
    renderPane();
    const sel = screen.getByTestId("term-font") as HTMLSelectElement;
    await userEvent.selectOptions(sel, "fira-code");
    expect(sel.value).toBe("fira-code");
    expect(loadTermSettings().fontFamily).toBe("fira-code");
  });

  it("changing font size updates the store", () => {
    renderPane();
    const input = screen.getByTestId("term-fontsize") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "16" } });
    expect(loadTermSettings().fontSize).toBe(16);
  });

  it("clamps font size to [8, 24]", () => {
    renderPane();
    const input = screen.getByTestId("term-fontsize") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "99" } });
    expect(loadTermSettings().fontSize).toBe(24);

    fireEvent.change(input, { target: { value: "1" } });
    expect(loadTermSettings().fontSize).toBe(8);
  });
});
