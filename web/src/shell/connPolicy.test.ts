import { describe, it, expect } from "vitest";
import { nextConnAction } from "./connPolicy";

describe("nextConnAction", () => {
  it("vanished -> drop", () => {
    expect(nextConnAction(undefined, true, "connected")).toBe("drop");
  });
  it("active without a ws -> open; with a ws -> none", () => {
    expect(nextConnAction("active", false, "waiting")).toBe("open");
    expect(nextConnAction("active", true, "connected")).toBe("none");
  });
  it("error -> error once, then none", () => {
    expect(nextConnAction("error", true, "connected")).toBe("error");
    expect(nextConnAction("error", false, "error")).toBe("none");
  });
  it("starting -> waiting once, then none", () => {
    expect(nextConnAction("starting", false, null)).toBe("waiting");
    expect(nextConnAction("starting", false, "waiting")).toBe("none");
  });
  it("suspended/unknown -> none", () => {
    expect(nextConnAction("suspended", false, null)).toBe("none");
    expect(nextConnAction("unreachable", false, null)).toBe("none");
  });
});
