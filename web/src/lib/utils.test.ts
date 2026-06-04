import { describe, it, expect } from "vitest";
import { cn } from "./utils";

describe("cn", () => {
  it("resolves conflicting tailwind classes, last wins", () => {
    expect(cn("px-2", "px-4")).toBe("px-4");
  });
  it("drops falsy values", () => {
    expect(cn("a", false, undefined, null)).toBe("a");
  });
});
