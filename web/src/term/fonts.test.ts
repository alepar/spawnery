import { describe, it, expect } from "vitest";
import { TERM_FONTS, fontById } from "./fonts";

describe("TERM_FONTS", () => {
  it("is non-empty", () => {
    expect(TERM_FONTS.length).toBeGreaterThan(0);
  });

  it("starts with the System monospace default", () => {
    expect(TERM_FONTS[0].id).toBe("system");
  });

  it("gives every entry an id, label, and non-empty stack", () => {
    for (const f of TERM_FONTS) {
      expect(typeof f.id).toBe("string");
      expect(f.id.length).toBeGreaterThan(0);
      expect(typeof f.label).toBe("string");
      expect(f.label.length).toBeGreaterThan(0);
      expect(typeof f.stack).toBe("string");
      expect(f.stack.length).toBeGreaterThan(0);
    }
  });

  it("has unique ids", () => {
    const ids = TERM_FONTS.map((f) => f.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("fontById", () => {
  it("returns the matching font", () => {
    const target = TERM_FONTS[1] ?? TERM_FONTS[0];
    expect(fontById(target.id)).toEqual(target);
  });

  it("falls back to the system default for unknown ids", () => {
    expect(fontById("does-not-exist").id).toBe("system");
    expect(fontById("")).toEqual(TERM_FONTS[0]);
  });
});
