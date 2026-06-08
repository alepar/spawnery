import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, beforeEach } from "vitest";
import type { ITheme } from "@xterm/xterm";
import { TERM_THEME_NAMES, TERM_THEMES } from "./themes.gen";
import { fontById } from "./fonts";
import {
  DEFAULT_TERM_SETTINGS,
  resolveActiveThemeName,
  resolveActiveTheme,
  applyToTerminal,
  loadTermSettings,
  TermSettingsProvider,
  useTermSettings,
  type TermSettings,
} from "./settings";

describe("resolveActiveThemeName", () => {
  const s: TermSettings = { ...DEFAULT_TERM_SETTINGS, follow: true, light: "L", dark: "D", fixed: "F" };

  it("follows app dark", () => {
    expect(resolveActiveThemeName(s, true)).toBe("D");
  });
  it("follows app light", () => {
    expect(resolveActiveThemeName(s, false)).toBe("L");
  });
  it("uses fixed when not following", () => {
    const fixed = { ...s, follow: false };
    expect(resolveActiveThemeName(fixed, true)).toBe("F");
    expect(resolveActiveThemeName(fixed, false)).toBe("F");
  });
});

describe("resolveActiveTheme", () => {
  it("returns the ITheme for a known name", () => {
    const s: TermSettings = { ...DEFAULT_TERM_SETTINGS, follow: false, fixed: "Dracula" };
    expect(resolveActiveTheme(s, false)).toBe(TERM_THEMES["Dracula"]);
  });

  it("falls back (never undefined) for an unknown name", () => {
    const s: TermSettings = { ...DEFAULT_TERM_SETTINGS, follow: false, fixed: "Definitely Not A Scheme" };
    const theme = resolveActiveTheme(s, false);
    expect(theme).toBeDefined();
    expect(typeof theme.background).toBe("string");
  });
});

describe("applyToTerminal", () => {
  it("sets theme, fontFamily (from fontById stack), and fontSize", () => {
    const term: { options: { theme?: ITheme; fontFamily?: string; fontSize?: number } } = { options: {} };
    const s: TermSettings = {
      ...DEFAULT_TERM_SETTINGS,
      follow: false,
      fixed: "Dracula",
      fontFamily: "fira-code",
      fontSize: 18,
    };
    applyToTerminal(term, s, false);
    expect(term.options.theme).toBe(TERM_THEMES["Dracula"]);
    expect(term.options.fontFamily).toBe(fontById("fira-code").stack);
    expect(term.options.fontSize).toBe(18);
  });
});

describe("loadTermSettings", () => {
  beforeEach(() => localStorage.clear());

  it("returns DEFAULT when storage is empty", () => {
    expect(loadTermSettings()).toEqual(DEFAULT_TERM_SETTINGS);
  });

  it("merges a stored partial over DEFAULT", () => {
    localStorage.setItem("term-settings", JSON.stringify({ fontSize: 20 }));
    const s = loadTermSettings();
    expect(s.fontSize).toBe(20);
    expect(s.fontFamily).toBe(DEFAULT_TERM_SETTINGS.fontFamily);
    expect(s.dark).toBe(DEFAULT_TERM_SETTINGS.dark);
  });

  it("ignores malformed JSON and returns DEFAULT", () => {
    localStorage.setItem("term-settings", "{not valid json");
    expect(loadTermSettings()).toEqual(DEFAULT_TERM_SETTINGS);
  });
});

describe("DEFAULT_TERM_SETTINGS scheme names", () => {
  it("light/dark/fixed are all present in TERM_THEME_NAMES", () => {
    expect(TERM_THEME_NAMES).toContain(DEFAULT_TERM_SETTINGS.light);
    expect(TERM_THEME_NAMES).toContain(DEFAULT_TERM_SETTINGS.dark);
    expect(TERM_THEME_NAMES).toContain(DEFAULT_TERM_SETTINGS.fixed);
  });
});

function Consumer() {
  const { settings, update, appDark } = useTermSettings();
  return (
    <div>
      <span data-testid="font-size">{settings.fontSize}</span>
      <span data-testid="app-dark">{String(appDark)}</span>
      <button data-testid="bump" onClick={() => update({ fontSize: 16 })}>
        bump
      </button>
    </div>
  );
}

describe("TermSettingsProvider / useTermSettings", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.className = "";
  });

  it("exposes DEFAULT settings", () => {
    render(
      <TermSettingsProvider>
        <Consumer />
      </TermSettingsProvider>,
    );
    expect(screen.getByTestId("font-size").textContent).toBe(String(DEFAULT_TERM_SETTINGS.fontSize));
  });

  it("update persists to localStorage and updates the value", async () => {
    render(
      <TermSettingsProvider>
        <Consumer />
      </TermSettingsProvider>,
    );
    await userEvent.click(screen.getByTestId("bump"));
    expect(screen.getByTestId("font-size").textContent).toBe("16");
    expect(JSON.parse(localStorage.getItem("term-settings")!).fontSize).toBe(16);
  });

  it("flips appDark on app-theme-change after the dark class toggles", () => {
    render(
      <TermSettingsProvider>
        <Consumer />
      </TermSettingsProvider>,
    );
    expect(screen.getByTestId("app-dark").textContent).toBe("false");
    act(() => {
      document.documentElement.classList.add("dark");
      window.dispatchEvent(new Event("app-theme-change"));
    });
    expect(screen.getByTestId("app-dark").textContent).toBe("true");
  });
});
