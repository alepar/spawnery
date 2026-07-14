import { describe, expect, it } from "vitest";
import { resolveIntentIssuedAt } from "./intent-time";

describe("resolveIntentIssuedAt", () => {
  it("applies stale and future offsets to the signing-time clock", () => {
    expect(resolveIntentIssuedAt({ issuedAtOffsetSeconds: -121 }, 1_000)).toBe(879);
    expect(resolveIntentIssuedAt({ issuedAtOffsetSeconds: 61 }, 1_000)).toBe(1_061);
  });

  it("preserves an explicit timestamp for non-relative mutations", () => {
    expect(resolveIntentIssuedAt({ issuedAt: 42, issuedAtOffsetSeconds: 61 }, 1_000)).toBe(42);
  });
});
