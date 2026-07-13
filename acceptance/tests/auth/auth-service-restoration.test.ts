import { describe, expect, it, vi } from "vitest";
import { restoreAuthService } from "./auth-service-restoration";

describe("restoreAuthService", () => {
  it("requires systemd active before probing a fresh session", async () => {
    const freshSession = vi.fn(async () => ({ accessToken: "fresh" }));
    await expect(restoreAuthService({
      start: async () => {},
      serviceState: async () => "activating",
      freshSession,
      wait: async () => {},
    }, 2)).rejects.toThrow("systemd state activating");
    expect(freshSession).not.toHaveBeenCalled();
  });

  it("surfaces a failed fresh-session health probe after bounded retries", async () => {
    await expect(restoreAuthService({
      start: async () => {},
      serviceState: async () => "active",
      freshSession: async () => { throw new Error("issuer unavailable"); },
      wait: async () => {},
    }, 2)).rejects.toThrow("fresh session health probe failed: issuer unavailable");
  });

  it("returns only a session issued after the service is active", async () => {
    const session = { accessToken: "fresh" };
    await expect(restoreAuthService({
      start: async () => {},
      serviceState: async () => "active",
      freshSession: async () => session,
      wait: async () => {},
    })).resolves.toBe(session);
  });
});
