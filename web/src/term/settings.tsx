// Terminal appearance settings: a localStorage-backed React store plus pure,
// tested helpers that map TermSettings -> an applied xterm terminal. Shared by
// the settings preview and the real TerminalView (later tasks). See the design
// spec §4 (docs/superpowers/specs/2026-06-08-terminal-appearance-settings-design.md).
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import type { ITheme } from "@xterm/xterm";
import { TERM_THEMES } from "./themes.gen";
import { fontById } from "./fonts";

export interface TermSettings {
  /** "Match app light/dark": when true, pick light/dark by appDark. */
  follow: boolean;
  /** Scheme name used when follow && !appDark. */
  light: string;
  /** Scheme name used when follow && appDark. */
  dark: string;
  /** Scheme name used when !follow. */
  fixed: string;
  /** A fonts.ts id ("system" default). */
  fontFamily: string;
  /** Font size in px. */
  fontSize: number;
}

export const DEFAULT_TERM_SETTINGS: TermSettings = {
  follow: true,
  light: "iTerm2 Solarized Light",
  dark: "Dracula",
  fixed: "Dracula",
  fontFamily: "system",
  fontSize: 13,
};

/** Safe fallback theme: prefer Dracula, else the first available scheme. */
const FALLBACK_THEME: ITheme =
  TERM_THEMES["Dracula"] ?? Object.values(TERM_THEMES)[0];

/** The scheme name that is active for the given settings + app light/dark. */
export function resolveActiveThemeName(s: TermSettings, appDark: boolean): string {
  return s.follow ? (appDark ? s.dark : s.light) : s.fixed;
}

/**
 * The xterm ITheme active for the given settings + app light/dark. Falls back to
 * a safe default when the resolved name is unknown, so it never returns undefined.
 */
export function resolveActiveTheme(s: TermSettings, appDark: boolean): ITheme {
  const name = resolveActiveThemeName(s, appDark);
  return TERM_THEMES[name] ?? FALLBACK_THEME;
}

/** Minimal structural target so this is testable and accepts a real Terminal. */
interface ApplyTarget {
  options: { theme?: ITheme; fontFamily?: string; fontSize?: number };
}

/**
 * Apply settings to a terminal's options. Does NOT load fonts or fit() —
 * callers handle that (font metrics must settle before measuring).
 */
export function applyToTerminal(
  term: ApplyTarget,
  s: TermSettings,
  appDark: boolean,
): void {
  term.options.theme = resolveActiveTheme(s, appDark);
  term.options.fontFamily = fontById(s.fontFamily).stack;
  term.options.fontSize = s.fontSize;
}

const STORAGE_KEY = "term-settings";

/**
 * Load settings from localStorage, merging the stored partial over DEFAULT so
 * adding a field later stays safe. Ignores parse/access errors.
 */
export function loadTermSettings(): TermSettings {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_TERM_SETTINGS };
    const stored = JSON.parse(raw) as Partial<TermSettings>;
    return { ...DEFAULT_TERM_SETTINGS, ...stored };
  } catch {
    return { ...DEFAULT_TERM_SETTINGS };
  }
}

/** Persist settings as JSON. Ignores quota/private-mode errors. */
export function saveTermSettings(s: TermSettings): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(s));
  } catch {
    /* quota exceeded / private mode */
  }
}

function readAppDark(): boolean {
  return document.documentElement.classList.contains("dark");
}

interface TermSettingsContextValue {
  settings: TermSettings;
  update: (patch: Partial<TermSettings>) => void;
  appDark: boolean;
}

const TermSettingsContext = createContext<TermSettingsContextValue | null>(null);

export function TermSettingsProvider({ children }: { children: ReactNode }) {
  const [settings, setSettings] = useState<TermSettings>(() => loadTermSettings());
  const [appDark, setAppDark] = useState<boolean>(() => readAppDark());

  const update = useCallback((patch: Partial<TermSettings>) => {
    setSettings((prev) => {
      const next = { ...prev, ...patch };
      saveTermSettings(next);
      return next;
    });
  }, []);

  useEffect(() => {
    const onThemeChange = () => setAppDark(readAppDark());
    window.addEventListener("app-theme-change", onThemeChange);
    return () => window.removeEventListener("app-theme-change", onThemeChange);
  }, []);

  return (
    <TermSettingsContext.Provider value={{ settings, update, appDark }}>
      {children}
    </TermSettingsContext.Provider>
  );
}

export function useTermSettings(): TermSettingsContextValue {
  const ctx = useContext(TermSettingsContext);
  if (!ctx) {
    throw new Error("useTermSettings must be used within a TermSettingsProvider");
  }
  return ctx;
}
