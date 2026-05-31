import { describe, it, expect, beforeEach } from "vitest";
import { setTheme } from "./theme";

describe("setTheme", () => {
  beforeEach(() => { document.documentElement.className = ""; localStorage.clear(); });

  it("adds .dark and persists when dark", () => {
    setTheme("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(localStorage.getItem("theme")).toBe("dark");
  });

  it("removes .dark and persists when light", () => {
    setTheme("dark");
    setTheme("light");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
    expect(localStorage.getItem("theme")).toBe("light");
  });
});
