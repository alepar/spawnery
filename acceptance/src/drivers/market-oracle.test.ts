import { describe, it, expect, vi, afterEach } from "vitest";
import { MarketOracle } from "./market-oracle";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("MarketOracle.listApps", () => {
  it("posts Connect-JSON to ListApps with the query body + bearer header and parses {apps}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          apps: [
            { id: "acc/app1", displayName: "App 1", latestVersion: "1.0.0", latestTier: "TRUST_TIER_UNVERIFIED", listed: true },
          ],
        }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const oracle = new MarketOracle("https://cp.example", "tok123");
    const apps = await oracle.listApps("app1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/ListApps");
    expect(init.method).toBe("POST");
    expect(init.headers["Content-Type"]).toBe("application/json");
    expect(init.headers["Connect-Protocol-Version"]).toBe("1");
    expect(init.headers["Authorization"]).toBe("Bearer tok123");
    expect(JSON.parse(init.body)).toEqual({ query: "app1" });

    expect(apps).toEqual([
      { id: "acc/app1", displayName: "App 1", latestVersion: "1.0.0", latestTier: "TRUST_TIER_UNVERIFIED", listed: true },
    ]);
  });

  it("defaults query to empty string and apps to [] when absent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const oracle = new MarketOracle("https://cp.example", "tok");
    expect(await oracle.listApps()).toEqual([]);
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init.body)).toEqual({ query: "" });
  });
});

describe("MarketOracle.getApp", () => {
  it("parses {app,versions,manifest}", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            app: { id: "acc/app1", displayName: "App 1", latestVersion: "1.0.0", listed: true },
            versions: [{ version: "1.0.0", ref: "acc/app1@sha", tier: "TRUST_TIER_UNVERIFIED", createdAt: "100" }],
            manifest: { apiVersion: "spawnery/v1", id: "acc/app1", title: "App 1" },
          }),
          { status: 200 },
        ),
      ),
    );
    const oracle = new MarketOracle("https://cp.example", "tok");
    const result = await oracle.getApp("acc/app1");
    expect(result.app).toEqual({ id: "acc/app1", displayName: "App 1", latestVersion: "1.0.0", latestTier: undefined, listed: true });
    expect(result.versions).toEqual([{ version: "1.0.0", ref: "acc/app1@sha", tier: "TRUST_TIER_UNVERIFIED", createdAt: "100" }]);
    expect(result.manifest).toEqual({ apiVersion: "spawnery/v1", id: "acc/app1", title: "App 1" });
  });

  it("defaults versions to [] when absent", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response(JSON.stringify({ app: { id: "a" } }), { status: 200 })),
    );
    const oracle = new MarketOracle("https://cp.example", "tok");
    const result = await oracle.getApp("a");
    expect(result.versions).toEqual([]);
    expect(result.manifest).toBeUndefined();
  });
});

describe("MarketOracle.listMyApps", () => {
  it("posts an empty body to ListMyApps and parses {apps}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ apps: [{ id: "acc/mine", listed: false }] }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const oracle = new MarketOracle("https://cp.example", "tok");
    const apps = await oracle.listMyApps();
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/ListMyApps");
    expect(JSON.parse(init.body)).toEqual({});
    expect(apps).toEqual([{ id: "acc/mine", displayName: "", latestVersion: "", latestTier: undefined, listed: false }]);
  });
});

describe("MarketOracle.registerAppVersion", () => {
  it("posts the manifest/version/ref and returns {appId,version,tier}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ appId: "acc/app1", version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const oracle = new MarketOracle("https://cp.example", "tok");
    const manifest = { apiVersion: "spawnery/v1", id: "acc/app1", title: "App 1" };
    const result = await oracle.registerAppVersion({ manifest, version: "1.0.0", ref: "acc/app1@sha" });

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/RegisterAppVersion");
    expect(JSON.parse(init.body)).toEqual({ manifest, version: "1.0.0", ref: "acc/app1@sha" });
    expect(result).toEqual({ appId: "acc/app1", version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" });
  });
});

describe("MarketOracle.setAppListing", () => {
  it("posts {appId,listed}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const oracle = new MarketOracle("https://cp.example", "tok");
    await oracle.setAppListing("acc/app1", false);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/SetAppListing");
    expect(JSON.parse(init.body)).toEqual({ appId: "acc/app1", listed: false });
  });
});

describe("MarketOracle — error surfacing", () => {
  it("surfaces a 403 as a thrown error naming the method", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("forbidden", { status: 403 })));
    const oracle = new MarketOracle("https://cp.example", "bad-token");
    await expect(oracle.listApps()).rejects.toThrow(/ListApps failed: 403/);
  });
});
