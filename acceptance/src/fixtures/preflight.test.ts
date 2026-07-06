import { describe, it, expect, vi, afterEach } from "vitest";
import { classifyPreflight, runPreflight, TargetDownError } from "./preflight";
import type { AppSummary } from "../drivers/oracle";
import type { TargetConfig } from "../config/target";

// runPreflight's `api` param is narrowed to Pick<AcceptanceClient, "listApps"> — stubbing that one
// method directly is simpler and more robust than routing a real AcceptanceClient (whose SDK
// transport runs on connect-web's own fetch-framing, not a bare Response mock) through vi.stubGlobal.
function stubApi(listApps: () => Promise<AppSummary[]>) {
  return { listApps: vi.fn(listApps) };
}

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
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 200 })));
    const api = stubApi(() => Promise.resolve([]));
    await expect(runPreflight(cfg, api)).resolves.toBeUndefined();
  });

  it("throws TargetDownError when the web origin is unreachable (network failure)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new Error("ECONNREFUSED")),
    );
    const api = stubApi(() => Promise.resolve([]));
    await expect(runPreflight(cfg, api)).rejects.toThrow(TargetDownError);
  });

  it("throws TargetDownError when the ListApps RPC fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 200 })));
    const api = stubApi(() => Promise.reject(new Error("boom")));
    await expect(runPreflight(cfg, api)).rejects.toThrow(TargetDownError);
  });
});
