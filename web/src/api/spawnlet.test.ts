import { describe, it, expect, vi, afterEach } from "vitest";
import { listSpawns, listAgentImages, createSpawn, renameSpawn, statusFromProto } from "./spawnlet";

function mockFetch(json: unknown) {
  const calls: { url: string; body: any }[] = [];
  const f = vi.fn(async (url: string, init: any) => {
    calls.push({ url, body: JSON.parse(init.body) });
    return { ok: true, json: async () => json, text: async () => "" } as any;
  });
  (globalThis as any).fetch = f;
  return calls;
}
afterEach(() => { vi.restoreAllMocks(); });

describe("statusFromProto", () => {
  it("maps proto enum names to short statuses", () => {
    expect(statusFromProto("SPAWN_STATUS_ACTIVE")).toBe("active");
    expect(statusFromProto("SPAWN_STATUS_SUSPENDED")).toBe("suspended");
    expect(statusFromProto("SPAWN_STATUS_ERROR")).toBe("error");
    expect(statusFromProto("SPAWN_STATUS_UNREACHABLE")).toBe("unreachable");
    expect(statusFromProto(undefined)).toBe("unknown");
    expect(statusFromProto("SPAWN_STATUS_BOGUS")).toBe("unknown");
  });
});

describe("listSpawns", () => {
  it("POSTs ListSpawns and maps the response to SpawnView[]", async () => {
    const calls = mockFetch({
      spawns: [
        { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "SPAWN_STATUS_ACTIVE" },
        { spawnId: "b", name: "", appId: "spawnery/zork", status: "SPAWN_STATUS_SUSPENDED" },
      ],
    });
    const out = await listSpawns();
    expect(calls[0].url).toContain("/cp.v1.SpawnService/ListSpawns");
    expect(out).toEqual([
      { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "" },
      { spawnId: "b", name: "", appId: "spawnery/zork", status: "suspended", mode: "" },
    ]);
  });
  it("maps mode from the response", async () => {
    mockFetch({
      spawns: [
        { spawnId: "c", name: "TUI", appId: "spawnery/goose", status: "SPAWN_STATUS_ACTIVE", mode: "tmux" },
      ],
    });
    const out = await listSpawns();
    expect(out[0].mode).toBe("tmux");
  });
  it("tolerates a missing spawns array", async () => {
    mockFetch({});
    expect(await listSpawns()).toEqual([]);
  });
});

describe("renameSpawn", () => {
  it("POSTs RenameSpawn with spawnId + name", async () => {
    const calls = mockFetch({});
    await renameSpawn("a", "New Name");
    expect(calls[0].url).toContain("/cp.v1.SpawnService/RenameSpawn");
    expect(calls[0].body).toEqual({ spawnId: "a", name: "New Name" });
  });
});

describe("listAgentImages", () => {
  it("POSTs ListAgentImages and maps images + runnables", async () => {
    const calls = mockFetch({
      images: [
        { image: "img:1", binaries: ["goose"], runnables: [
          { id: "goose-acp", label: "Goose · rich web", mode: "acp" },
          { id: "goose-tui", label: "Goose · terminal", mode: "tmux" },
        ] },
      ],
    });
    const out = await listAgentImages();
    expect(calls[0].url).toContain("/cp.v1.SpawnService/ListAgentImages");
    expect(out).toEqual([
      { image: "img:1", runnables: [
        { id: "goose-acp", label: "Goose · rich web", mode: "acp" },
        { id: "goose-tui", label: "Goose · terminal", mode: "tmux" },
      ] },
    ]);
  });
  it("tolerates missing arrays", async () => {
    mockFetch({});
    expect(await listAgentImages()).toEqual([]);
  });
});

describe("createSpawn", () => {
  it("POSTs CreateSpawn with the agent selection", async () => {
    const calls = mockFetch({ spawnId: "sp1" });
    const id = await createSpawn("spawnery/wiki", "m", "img:1", "goose-acp");
    expect(id).toBe("sp1");
    expect(calls[0].body).toEqual({ appId: "spawnery/wiki", model: "m", image: "img:1", runnableId: "goose-acp" });
  });
  it("defaults selection to empty (legacy)", async () => {
    const calls = mockFetch({ spawnId: "sp2" });
    await createSpawn("spawnery/wiki", "m");
    expect(calls[0].body).toEqual({ appId: "spawnery/wiki", model: "m", image: "", runnableId: "" });
  });
});
