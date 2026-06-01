import { describe, it, expect, vi, beforeEach } from "vitest";
import { listApps, getApp, listMyApps, registerAppVersion, setAppListing, tierLabel } from "./catalog";

function mockFetch(json: unknown, ok = true) {
  return vi.fn().mockResolvedValue({ ok, status: ok ? 200 : 400, json: async () => json, text: async () => JSON.stringify(json) });
}

describe("catalog api", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("listApps posts query and maps apps", async () => {
    const f = mockFetch({ apps: [{ id: "a/b", displayName: "B", latestTier: "TRUST_TIER_REVIEWED", listed: true }] });
    vi.stubGlobal("fetch", f);
    const apps = await listApps("wiki");
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/ListApps", expect.objectContaining({ method: "POST" }));
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body).toEqual({ query: "wiki" });
    expect(apps[0].id).toBe("a/b");
  });

  it("listApps tolerates missing apps", async () => {
    vi.stubGlobal("fetch", mockFetch({}));
    expect(await listApps()).toEqual([]);
  });

  it("getApp returns app+versions+manifest", async () => {
    vi.stubGlobal("fetch", mockFetch({ app: { id: "a/b" }, versions: [{ version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" }], manifest: { id: "a/b", title: "B" } }));
    const r = await getApp("a/b");
    expect(r.app.id).toBe("a/b");
    expect(r.versions[0].version).toBe("1.0.0");
    expect(r.manifest?.title).toBe("B");
  });

  it("setAppListing posts appId+listed", async () => {
    const f = mockFetch({});
    vi.stubGlobal("fetch", f);
    await setAppListing("a/b", false);
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ appId: "a/b", listed: false });
  });

  it("registerAppVersion posts manifest+version+ref", async () => {
    const f = mockFetch({ appId: "a/b", version: "1.0.0", tier: "TRUST_TIER_UNVERIFIED" });
    vi.stubGlobal("fetch", f);
    const r = await registerAppVersion({ manifest: { apiVersion: "spawnery/v1", id: "a/b", title: "B", visibility: "open", mounts: [{ name: "main", path: "data", seed: "seed" }] } as any, version: "1.0.0", ref: "a/b@sha" });
    expect(r.tier).toBe("TRUST_TIER_UNVERIFIED");
  });

  it("listMyApps maps apps", async () => {
    vi.stubGlobal("fetch", mockFetch({ apps: [{ id: "a/b", listed: false }] }));
    expect((await listMyApps())[0].listed).toBe(false);
  });

  it("tierLabel maps enum names", () => {
    expect(tierLabel("TRUST_TIER_REVIEWED").label).toBe("reviewed");
    expect(tierLabel("TRUST_TIER_SCANNED").label).toBe("scanned");
    expect(tierLabel("TRUST_TIER_UNVERIFIED").label).toBe("unverified");
    expect(tierLabel(undefined).label).toBe("—");
  });
});
