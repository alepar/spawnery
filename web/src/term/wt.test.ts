import { describe, it, expect } from "vitest";
import { wtToITheme } from "./wt";

// A minimal-but-complete Windows-Terminal scheme.
const sample = {
  name: "Sample",
  background: "#282a36",
  foreground: "#f8f8f2",
  cursorColor: "#bbbbbb",
  selectionBackground: "#44475a",
  black: "#000000",
  red: "#ff0000",
  green: "#00ff00",
  yellow: "#ffff00",
  blue: "#0000ff",
  purple: "#aa00aa",
  cyan: "#00ffff",
  white: "#ffffff",
  brightBlack: "#111111",
  brightRed: "#ff1111",
  brightGreen: "#11ff11",
  brightYellow: "#ffff11",
  brightBlue: "#1111ff",
  brightPurple: "#cc00cc",
  brightCyan: "#11ffff",
  brightWhite: "#eeeeee",
};

describe("wtToITheme", () => {
  it("maps core + ANSI fields, including purple -> magenta", () => {
    const t = wtToITheme(sample);
    expect(t.background).toBe("#282a36");
    expect(t.foreground).toBe("#f8f8f2");
    expect(t.cursor).toBe("#bbbbbb");
    // cursorAccent + selectionInactiveBackground are derived.
    expect(t.cursorAccent).toBe("#282a36");
    expect(t.selectionBackground).toBe("#44475a");
    expect(t.selectionInactiveBackground).toBe("#44475a");
    // The WT "purple" rename is the load-bearing detail.
    expect(t.magenta).toBe("#aa00aa");
    expect(t.brightMagenta).toBe("#cc00cc");
    // Sanity on a few same-name maps.
    expect(t.black).toBe("#000000");
    expect(t.brightWhite).toBe("#eeeeee");
    // selectionForeground is intentionally omitted.
    expect(t.selectionForeground).toBeUndefined();
  });

  it("does not carry the WT-only keys (name/purple/cursorColor) into the theme", () => {
    const t = wtToITheme(sample) as Record<string, unknown>;
    expect(t.name).toBeUndefined();
    expect(t.purple).toBeUndefined();
    expect(t.brightPurple).toBeUndefined();
    expect(t.cursorColor).toBeUndefined();
  });

  it("throws when a required field is missing", () => {
    const { red: _red, ...missing } = sample;
    expect(() => wtToITheme(missing)).toThrow(/red/);
  });

  it("throws when a field is not a #rrggbb hex", () => {
    expect(() => wtToITheme({ ...sample, blue: "blue" })).toThrow(/blue/);
    expect(() => wtToITheme({ ...sample, green: "#0f0" })).toThrow(/green/);
    expect(() => wtToITheme({ ...sample, background: 123 })).toThrow(/background/);
  });

  it("throws on a non-object input", () => {
    expect(() => wtToITheme(null)).toThrow();
    expect(() => wtToITheme("nope")).toThrow();
  });
});
