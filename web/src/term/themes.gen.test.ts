import { describe, it, expect } from "vitest";
import type { ITheme } from "@xterm/xterm";
import { TERM_THEMES, TERM_THEME_NAMES } from "./themes.gen";

const HEX = /^#[0-9a-fA-F]{6}$/;

// Every ITheme key the corpus is required to populate.
const REQUIRED_KEYS: (keyof ITheme)[] = [
  "background",
  "foreground",
  "cursor",
  "cursorAccent",
  "selectionBackground",
  "selectionInactiveBackground",
  "black",
  "red",
  "green",
  "yellow",
  "blue",
  "magenta",
  "cyan",
  "white",
  "brightBlack",
  "brightRed",
  "brightGreen",
  "brightYellow",
  "brightBlue",
  "brightMagenta",
  "brightCyan",
  "brightWhite",
];

describe("themes.gen", () => {
  it("ships a non-empty corpus (~30 curated schemes)", () => {
    expect(TERM_THEME_NAMES.length).toBeGreaterThanOrEqual(25);
    expect(TERM_THEME_NAMES.length).toBe(Object.keys(TERM_THEMES).length);
  });

  it("TERM_THEME_NAMES is sorted and matches Object.keys(TERM_THEMES)", () => {
    const keys = Object.keys(TERM_THEMES);
    const sorted = [...TERM_THEME_NAMES].sort((a, b) => a.localeCompare(b, "en"));
    expect(TERM_THEME_NAMES).toEqual(sorted);
    expect(TERM_THEME_NAMES).toEqual(keys);
  });

  it("every theme has all required ITheme keys with valid #rrggbb values", () => {
    for (const name of TERM_THEME_NAMES) {
      const theme = TERM_THEMES[name] as Record<string, unknown>;
      for (const key of REQUIRED_KEYS) {
        const v = theme[key];
        expect(typeof v, `${name}.${key} type`).toBe("string");
        expect(HEX.test(v as string), `${name}.${key} = ${String(v)}`).toBe(true);
      }
    }
  });

  it("includes a few well-known schemes", () => {
    expect(TERM_THEME_NAMES).toContain("Dracula");
    expect(TERM_THEME_NAMES).toContain("Nord");
  });
});
