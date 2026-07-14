import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

describe("tenancy CLI credential custody", () => {
  it("does not read or decode spawnctl's stored credentials", () => {
    const source = readFileSync(new URL("./non-leakage.spec.ts", import.meta.url), "utf8");
    expect(source).not.toContain("readStoredCliIdentity");
    expect(source).not.toContain("auth.json");
    expect(source).not.toContain("cp_access_token");
    expect(source).not.toMatch(/\bfromBinary\b|\bauthv1\b/);
  });
});
