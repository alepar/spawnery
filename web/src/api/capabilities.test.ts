import { describe, it, expect } from "vitest";
import { capabilityFor, AGENTS } from "./capabilities";

describe("capabilities", () => {
  it("claude supports skill", () => {
    expect(capabilityFor("skill", "claude")).toBe("supported");
  });

  it("opencode skill is no-op", () => {
    expect(capabilityFor("skill", "opencode")).toBe("no-op");
  });

  it("opencode plugin is best-effort", () => {
    expect(capabilityFor("plugin", "opencode")).toBe("best-effort");
  });

  it("unknown agent returns no-op", () => {
    expect(capabilityFor("skill", "unknown-agent")).toBe("no-op");
  });

  it("unknown kind returns no-op", () => {
    expect(capabilityFor("unknown-kind", "claude")).toBe("no-op");
  });

  it("AGENTS list is non-empty and contains claude", () => {
    expect(AGENTS.length).toBeGreaterThan(0);
    expect(AGENTS).toContain("claude");
  });

  it("AGENTS list contains opencode", () => {
    expect(AGENTS).toContain("opencode");
  });
});
