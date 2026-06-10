import { describe, it, expect } from "vitest";
import { mergeMode } from "./mode";
import type { ModePayload } from "@/acp/frames";

const advert: ModePayload = {
  current: "build",
  available: [
    { id: "build", name: "Build" },
    { id: "plan", name: "Plan" },
  ],
};

describe("mergeMode", () => {
  it("adopts the full advertisement (current + available) from null", () => {
    expect(mergeMode(null, advert)).toEqual(advert);
  });

  it("keeps the prior available set when a current_mode_update carries only current", () => {
    const merged = mergeMode(advert, { current: "plan" });
    expect(merged).toEqual({ current: "plan", available: advert.available });
  });

  it("replaces available when a new advertisement carries a non-empty set", () => {
    const next: ModePayload = { current: "x", available: [{ id: "x", name: "X" }] };
    expect(mergeMode(advert, next)).toEqual(next);
  });

  it("leaves state unchanged for an undefined frame payload", () => {
    expect(mergeMode(advert, undefined)).toEqual(advert);
  });
});
