import { afterEach, describe, expect, it, vi } from "vitest";

// endpoints.ts captures import.meta.env at module load, so each case stubs the env and
// re-imports a fresh module instance.
async function load(env: { VITE_CP_ORIGIN?: string; VITE_AS_ORIGIN?: string }) {
  vi.resetModules();
  vi.stubEnv("VITE_CP_ORIGIN", env.VITE_CP_ORIGIN ?? "");
  vi.stubEnv("VITE_AS_ORIGIN", env.VITE_AS_ORIGIN ?? "");
  return await import("./endpoints");
}

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("cpWsUrl", () => {
  it("derives wss:// from a configured https CP origin", async () => {
    const m = await load({ VITE_CP_ORIGIN: "https://cp.spawnery.dev" });
    expect(m.cpWsUrl("/ws/session")).toBe("wss://cp.spawnery.dev/ws/session");
  });

  it("derives ws:// from a configured http CP origin", async () => {
    const m = await load({ VITE_CP_ORIGIN: "http://127.0.0.1:8080" });
    expect(m.cpWsUrl("/ws/session")).toBe("ws://127.0.0.1:8080/ws/session");
  });

  it("falls back to location.origin in dev (no configured origin)", async () => {
    const m = await load({});
    // jsdom's location.origin is http://localhost:3000 by default.
    expect(m.cpWsUrl("/ws/session")).toBe(
      window.location.origin.replace(/^http/, "ws") + "/ws/session",
    );
    expect(m.cpWsUrl("/ws/session")).toMatch(/^ws:\/\//);
  });
});

describe("cpHttpUrl / asHttpUrl", () => {
  it("joins configured origins with the path", async () => {
    const m = await load({
      VITE_CP_ORIGIN: "https://cp.spawnery.dev",
      VITE_AS_ORIGIN: "https://as.spawnery.dev",
    });
    expect(m.cpHttpUrl("/cp.v1.SpawnService/ListSpawns")).toBe(
      "https://cp.spawnery.dev/cp.v1.SpawnService/ListSpawns",
    );
    expect(m.asHttpUrl("/healthz")).toBe("https://as.spawnery.dev/healthz");
  });

  it("stays relative in dev (vite proxy)", async () => {
    const m = await load({});
    expect(m.cpHttpUrl("/cp.v1.SpawnService/ListSpawns")).toBe("/cp.v1.SpawnService/ListSpawns");
    expect(m.asHttpUrl("/enroll")).toBe("/enroll");
  });
});
