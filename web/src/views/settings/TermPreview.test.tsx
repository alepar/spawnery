import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";

globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
(document as any).fonts = { load: vi.fn().mockResolvedValue([]) };

// Single captured Terminal instance so tests can inspect its options/write.
const writes: (string | Uint8Array)[] = [];
const mockTerminal = {
  loadAddon: vi.fn(),
  open: vi.fn(),
  write: vi.fn((d: string | Uint8Array) => { writes.push(d); }),
  refresh: vi.fn(),
  dispose: vi.fn(),
  options: {} as Record<string, unknown>,
  rows: 24,
  cols: 80,
};

vi.mock("@xterm/xterm", () => ({
  Terminal: vi.fn(() => mockTerminal),
}));
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: vi.fn(() => ({ fit: vi.fn(), dispose: vi.fn() })),
}));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

import { TermPreview } from "./TermPreview";
import { TermSettingsProvider, DEFAULT_TERM_SETTINGS } from "@/term/settings";
import { TERM_THEMES } from "@/term/themes.gen";
import { fontById } from "@/term/fonts";

describe("TermPreview", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.className = "";
    writes.length = 0;
    mockTerminal.options = {};
    mockTerminal.write.mockClear();
    mockTerminal.refresh.mockClear();
  });

  it("renders the host div", () => {
    render(
      <TermSettingsProvider>
        <TermPreview />
      </TermSettingsProvider>,
    );
    expect(screen.getByTestId("term-preview")).toBeTruthy();
  });

  it("applies settings to the terminal options on mount", async () => {
    render(
      <TermSettingsProvider>
        <TermPreview />
      </TermSettingsProvider>,
    );
    // Defaults: follow=true, app is light -> the light scheme is active.
    await waitFor(() => {
      expect(mockTerminal.options.theme).toBe(TERM_THEMES[DEFAULT_TERM_SETTINGS.light]);
    });
    expect(mockTerminal.options.fontFamily).toBe(fontById(DEFAULT_TERM_SETTINGS.fontFamily).stack);
    expect(mockTerminal.options.fontSize).toBe(DEFAULT_TERM_SETTINGS.fontSize);
  });

  it("writes the sample buffer once on mount", () => {
    render(
      <TermSettingsProvider>
        <TermPreview />
      </TermSettingsProvider>,
    );
    expect(mockTerminal.write).toHaveBeenCalled();
    // The sample includes a prompt and ANSI escapes.
    expect(writes.join("")).toContain("user@spawnery");
    expect(writes.join("")).toContain("\x1b[");
  });

  it("refreshes after the font loads", async () => {
    render(
      <TermSettingsProvider>
        <TermPreview />
      </TermSettingsProvider>,
    );
    await waitFor(() => {
      expect(mockTerminal.refresh).toHaveBeenCalled();
    });
  });
});
