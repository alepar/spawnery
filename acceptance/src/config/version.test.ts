import { describe, it, expect } from "vitest";
import { assertVersionPin, VersionSkewError } from "./version";

describe("assertVersionPin", () => {
  it("does not throw when refs are equal", () => {
    expect(() => assertVersionPin("dev", "dev")).not.toThrow();
  });

  it("throws VersionSkewError when refs differ", () => {
    expect(() => assertVersionPin("dev", "staging")).toThrow(VersionSkewError);
  });

  it("error message names both refs", () => {
    try {
      assertVersionPin("abc123", "def456");
      expect.unreachable();
    } catch (e) {
      expect(e).toBeInstanceOf(VersionSkewError);
      expect((e as Error).message).toContain("abc123");
      expect((e as Error).message).toContain("def456");
    }
  });
});
