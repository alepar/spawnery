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
  it("unreachable (dead spawn) -> error like a failed spawn", () => {
    expect(nextConnAction("unreachable", true, "connected")).toBe("error");
    expect(nextConnAction("unreachable", false, "error")).toBe("none");
  });
  it("starting -> waiting once, then none", () => {
    expect(nextConnAction("starting", false, null)).toBe("waiting");
    expect(nextConnAction("starting", false, "waiting")).toBe("none");
  });
  it("suspended/suspending/unknown -> none", () => {
    expect(nextConnAction("suspended", false, null)).toBe("none");
    expect(nextConnAction("suspending", false, null)).toBe("none");
    expect(nextConnAction("unknown", false, null)).toBe("none");
  });
  it("active + live ws stays none regardless of reconnecting/disconnected conn", () => {
    expect(nextConnAction("active", true, "reconnecting")).toBe("none");
    expect(nextConnAction("active", true, "disconnected")).toBe("none");
  });
  it("a dead node mid-reconnect is torn down by the poll (error), not left spinning", () => {
    expect(nextConnAction("unreachable", true, "disconnected")).toBe("error");
    expect(nextConnAction("error", true, "reconnecting")).toBe("error");
  });
});
