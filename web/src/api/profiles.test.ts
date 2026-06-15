import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  listProfiles,
  createProfile,
  getProfile,
  updateProfile,
  deleteProfile,
  addProfileEntry,
  removeProfileEntry,
  listCatalogEntries,
  getCatalogEntry,
  kindToCapKind,
  KIND_LABEL,
  type ProfileEntryKind,
} from "./profiles";

function mockFetch(json: unknown, ok = true) {
  return vi.fn().mockResolvedValue({
    ok,
    status: ok ? 200 : 400,
    json: async () => json,
    text: async () => JSON.stringify(json),
  });
}

describe("profiles api", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("listProfiles POSTs ListProfiles and maps profiles", async () => {
    const f = mockFetch({ profiles: [{ profileId: "p1", name: "My Profile", version: 1, updatedAt: "" }] });
    vi.stubGlobal("fetch", f);
    const profiles = await listProfiles();
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/ListProfiles", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({});
    expect(profiles[0].profileId).toBe("p1");
  });

  it("listProfiles tolerates missing profiles array", async () => {
    vi.stubGlobal("fetch", mockFetch({}));
    expect(await listProfiles()).toEqual([]);
  });

  it("createProfile POSTs CreateProfile with name", async () => {
    const f = mockFetch({ profileId: "p2", version: 1 });
    vi.stubGlobal("fetch", f);
    const r = await createProfile("My Profile");
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/CreateProfile", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ name: "My Profile" });
    expect(r.profileId).toBe("p2");
    expect(r.version).toBe(1);
  });

  it("getProfile POSTs GetProfile and normalizes missing arrays", async () => {
    const f = mockFetch({ profile: { profileId: "p1", name: "My Profile", version: 2 } });
    vi.stubGlobal("fetch", f);
    const profile = await getProfile("p1");
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ profileId: "p1" });
    expect(profile.profileId).toBe("p1");
    expect(profile.entries).toEqual([]);
    expect(profile.secretIds).toEqual([]);
  });

  it("getProfile preserves entries and secretIds when present", async () => {
    const entry = {
      entryId: "e1",
      kind: "PROFILE_ENTRY_KIND_SKILL",
      name: "My Skill",
      source: "PROFILE_ENTRY_SOURCE_CATALOG_REF",
      catalogId: "cat1",
    };
    vi.stubGlobal("fetch", mockFetch({ profile: { profileId: "p1", name: "N", version: 3, entries: [entry], secretIds: ["s1"] } }));
    const profile = await getProfile("p1");
    expect(profile.entries).toHaveLength(1);
    expect(profile.entries[0].entryId).toBe("e1");
    expect(profile.secretIds).toEqual(["s1"]);
  });

  it("updateProfile POSTs UpdateProfile with CAS fields", async () => {
    const f = mockFetch({ version: 3 });
    vi.stubGlobal("fetch", f);
    const r = await updateProfile("p1", 2, "Renamed");
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ profileId: "p1", expectedVersion: 2, name: "Renamed" });
    expect(r.version).toBe(3);
  });

  it("deleteProfile POSTs DeleteProfile", async () => {
    const f = mockFetch({});
    vi.stubGlobal("fetch", f);
    await deleteProfile("p1");
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/DeleteProfile", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ profileId: "p1" });
  });

  it("addProfileEntry POSTs AddProfileEntry with CAS and entry", async () => {
    const f = mockFetch({ entryId: "e1", version: 2 });
    vi.stubGlobal("fetch", f);
    const r = await addProfileEntry("p1", 1, {
      kind: "PROFILE_ENTRY_KIND_SKILL",
      name: "My Skill",
      source: "PROFILE_ENTRY_SOURCE_CATALOG_REF",
      catalogId: "cat1",
    });
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body.profileId).toBe("p1");
    expect(body.expectedVersion).toBe(1);
    expect(body.entry.kind).toBe("PROFILE_ENTRY_KIND_SKILL");
    expect(r.entryId).toBe("e1");
    expect(r.version).toBe(2);
  });

  it("removeProfileEntry POSTs RemoveProfileEntry", async () => {
    const f = mockFetch({ version: 3 });
    vi.stubGlobal("fetch", f);
    const r = await removeProfileEntry("p1", 2, "e1");
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body).toEqual({ profileId: "p1", expectedVersion: 2, entryId: "e1" });
    expect(r.version).toBe(3);
  });

  it("listCatalogEntries POSTs ListCatalogEntries", async () => {
    const f = mockFetch({ entries: [{ catalogId: "c1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "Skill1" }] });
    vi.stubGlobal("fetch", f);
    const entries = await listCatalogEntries();
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({});
    expect(entries[0].catalogId).toBe("c1");
  });

  it("listCatalogEntries tolerates missing entries array", async () => {
    vi.stubGlobal("fetch", mockFetch({}));
    expect(await listCatalogEntries()).toEqual([]);
  });

  it("getCatalogEntry POSTs GetCatalogEntry with catalogId", async () => {
    const f = mockFetch({ entry: { catalogId: "c1", kind: "PROFILE_ENTRY_KIND_MCP", name: "My MCP", content: "..." } });
    vi.stubGlobal("fetch", f);
    const entry = await getCatalogEntry("c1");
    expect(JSON.parse((f.mock.calls[0][1] as any).body)).toEqual({ catalogId: "c1" });
    expect(entry.catalogId).toBe("c1");
  });
});

describe("kindToCapKind", () => {
  const cases: [ProfileEntryKind, string][] = [
    ["PROFILE_ENTRY_KIND_SKILL", "skill"],
    ["PROFILE_ENTRY_KIND_MCP", "mcp"],
    ["PROFILE_ENTRY_KIND_CONFIG", "config"],
    ["PROFILE_ENTRY_KIND_PLUGIN", "plugin"],
  ];
  for (const [kind, expected] of cases) {
    it(`${kind} -> ${expected}`, () => expect(kindToCapKind(kind)).toBe(expected));
  }
});

describe("KIND_LABEL", () => {
  it("maps all kinds to display labels", () => {
    expect(KIND_LABEL["PROFILE_ENTRY_KIND_SKILL"]).toBe("Skill");
    expect(KIND_LABEL["PROFILE_ENTRY_KIND_MCP"]).toBe("MCP");
    expect(KIND_LABEL["PROFILE_ENTRY_KIND_CONFIG"]).toBe("Config");
    expect(KIND_LABEL["PROFILE_ENTRY_KIND_PLUGIN"]).toBe("Plugin");
  });
});
