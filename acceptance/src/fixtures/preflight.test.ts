import { describe, it, expect, vi, afterEach } from "vitest";
import { classifyPreflight, runPreflight, TargetDownError } from "./preflight";
import { ApiDriver } from "../drivers/api";
import type { TargetConfig } from "../config/target";

describe("classifyPreflight", () => {
  it("is ok only when both web and cp are reachable", () => {
    expect(classifyPreflight({ webOk: true, cpOk: true })).toBe("ok");
  });
  it("is target-down when web is unreachable", () => {
    expect(classifyPreflight({ webOk: false, cpOk: true })).toBe("target-down");
  });
  it("is target-down when cp is unreachable", () => {
    expect(classifyPreflight({ webOk: true, cpOk: false })).toBe("target-down");
  });
  it("is target-down when both are unreachable", () => {
    expect(classifyPreflight({ webOk: false, cpOk: false })).toBe("target-down");
  });
});

const cfg = { webOrigin: "https://target.example", cpEndpoint: "https://target.example" } as TargetConfig;

describe("runPreflight", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("resolves when both the web GET and the ListApps RPC succeed", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string) => {
        if (url === cfg.webOrigin) return Promise.resolve(new Response("", { status: 200 }));
        return Promise.resolve(new Response(JSON.stringify({ apps: [] }), { status: 200 }));
      }),
    );
    const api = new ApiDriver(cfg.cpEndpoint, "tok");
    await expect(runPreflight(cfg, api)).resolves.toBeUndefined();
  });

  it("throws TargetDownError when the web origin is unreachable (network failure)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("ECONNREFUSED")),
    );
    const api = new ApiDriver(cfg.cpEndpoint, "tok");
    await expect(runPreflight(cfg, api)).rejects.toThrow(TargetDownError);
  });

  it("throws TargetDownError when the ListApps RPC fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string) => {
        if (url === cfg.webOrigin) return Promise.resolve(new Response("", { status: 200 }));
        return Promise.resolve(new Response("boom", { status: 500 }));
      }),
    );
    const api = new ApiDriver(cfg.cpEndpoint, "tok");
    await expect(runPreflight(cfg, api)).rejects.toThrow(TargetDownError);
  });
});
