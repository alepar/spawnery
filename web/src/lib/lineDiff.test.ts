import { describe, it, expect } from "vitest";
import { lineDiff, LINE_DIFF_CAP } from "./lineDiff";

describe("lineDiff", () => {
  it("identical text produces all-context lines", () => {
    const text = "a\nb\nc";
    const d = lineDiff(text, text);
    expect(d).toEqual([
      { type: "ctx", text: "a" },
      { type: "ctx", text: "b" },
      { type: "ctx", text: "c" },
    ]);
  });

  it("pure insert: old is a prefix of new", () => {
    const d = lineDiff("a\nb", "a\nb\nc\nd");
    expect(d).toEqual([
      { type: "ctx", text: "a" },
      { type: "ctx", text: "b" },
      { type: "add", text: "c" },
      { type: "add", text: "d" },
    ]);
  });

  it("pure delete: new is a prefix of old", () => {
    const d = lineDiff("a\nb\nc\nd", "a\nb");
    expect(d).toEqual([
      { type: "ctx", text: "a" },
      { type: "ctx", text: "b" },
      { type: "del", text: "c" },
      { type: "del", text: "d" },
    ]);
  });

  it("replace in the middle", () => {
    const d = lineDiff("a\nb\nc\nd", "a\nx\ny\nd");
    expect(d).toEqual([
      { type: "ctx", text: "a" },
      { type: "del", text: "b" },
      { type: "del", text: "c" },
      { type: "add", text: "x" },
      { type: "add", text: "y" },
      { type: "ctx", text: "d" },
    ]);
  });

  it("empty old text (ADDED member) is all add lines", () => {
    const d = lineDiff("", "a\nb");
    expect(d).toEqual([
      { type: "add", text: "a" },
      { type: "add", text: "b" },
    ]);
  });

  it("empty new text (REMOVED member) is all del lines", () => {
    const d = lineDiff("a\nb", "");
    expect(d).toEqual([
      { type: "del", text: "a" },
      { type: "del", text: "b" },
    ]);
  });

  it("both empty produces no lines", () => {
    expect(lineDiff("", "")).toEqual([]);
  });

  it("caps each side at LINE_DIFF_CAP lines and marks the rest elided", () => {
    const big = Array.from({ length: LINE_DIFF_CAP + 50 }, (_, i) => `line${i}`).join("\n");
    const d = lineDiff(big, big);
    // capped to LINE_DIFF_CAP context lines + one elision marker
    expect(d.length).toBe(LINE_DIFF_CAP + 1);
    expect(d.slice(0, LINE_DIFF_CAP)).toEqual(
      Array.from({ length: LINE_DIFF_CAP }, (_, i) => ({ type: "ctx", text: `line${i}` })),
    );
    const last = d[d.length - 1];
    expect(last.type).toBe("elided");
    expect(last.text).toContain("50");
  });
});
