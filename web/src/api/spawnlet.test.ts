import { describe, it, expect, vi, afterEach } from "vitest";
import { listSpawns, listAgentImages, createSpawn, renameSpawn, statusFromProto, setSpawnModel, spawnLifecycleAction, recreateSpawn } from "./spawnlet";
import type { SpawnStatus, SpawnLifecycleAction } from "./spawnlet";

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
      { spawnId: "a", name: "Wiki", appId: "spawnery/wiki", status: "active", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "", parentSpawnId: "", forkedAt: 0 },
      { spawnId: "b", name: "", appId: "spawnery/zork", status: "suspended", mode: "", model: "", modelApplied: true, journalKeyDeliveryPending: false, transitionPhase: "", parentSpawnId: "", forkedAt: 0 },
    ]);
  });
  it("maps fork lineage fields from ListSpawns", async () => {
    mockFetch({
      spawns: [{
        spawnId: "fork-1",
        name: "Trial branch",
        appId: "spawnery/wiki",
        status: "SPAWN_STATUS_STARTING",
        parentSpawnId: "source-1",
        forkedAt: "1780000000",
      }],
    });
    const out = await listSpawns();
    expect(out[0].parentSpawnId).toBe("source-1");
    expect(out[0].forkedAt).toBe(1780000000);
  });
  it("maps model and modelApplied from the response", async () => {
    mockFetch({
      spawns: [
        { spawnId: "d", appId: "spawnery/wiki", status: "SPAWN_STATUS_ACTIVE", model: "openai/gpt-4o", modelApplied: false },
      ],
    });
    const out = await listSpawns();
    expect(out[0].model).toBe("openai/gpt-4o");
    expect(out[0].modelApplied).toBe(false);
  });
  it("defaults model to '' and modelApplied to true when absent", async () => {
    mockFetch({ spawns: [{ spawnId: "e", appId: "a", status: "SPAWN_STATUS_ACTIVE" }] });
    const out = await listSpawns();
    expect(out[0].model).toBe("");
    expect(out[0].modelApplied).toBe(true);
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

describe("spawnLifecycleAction", () => {
  it("maps every status to the lifecycle action the CP will accept", () => {
    const cases: Array<[SpawnStatus, SpawnLifecycleAction]> = [
      ["active", { kind: "suspend", label: "Suspend" }],
      ["suspended", { kind: "resume", label: "Resume" }],
      ["unreachable", { kind: "recreate", label: "Recreate" }],
      ["error", { kind: "recreate", label: "Recreate" }],
      ["starting", { kind: "pending", label: "Starting…" }],
      ["suspending", { kind: "pending", label: "Suspending…" }],
      ["resuming", { kind: "pending", label: "Resuming…" }],
      ["unknown", { kind: "pending", label: "Unavailable" }],
    ];
    for (const [status, want] of cases) {
      expect(spawnLifecycleAction(status), status).toEqual(want);
    }
  });
  it("uses seeding copy for fork children that are still starting", () => {
    expect(spawnLifecycleAction("starting", undefined, true)).toEqual({ kind: "pending", label: "Seeding…" });
  });
  it("includes phase in suspending/resuming labels when transitionPhase is set", () => {
    expect(spawnLifecycleAction("suspending", "snapshot")).toEqual({ kind: "pending", label: "Suspending: snapshot" });
    expect(spawnLifecycleAction("resuming", "attaching")).toEqual({ kind: "pending", label: "Resuming: attaching" });
  });
});

describe("recreateSpawn", () => {
  it("POSTs RecreateSpawn with spawnId", async () => {
    const calls = mockFetch({});
    await recreateSpawn("a");
    expect(calls[0].url).toContain("/cp.v1.SpawnService/RecreateSpawn");
    expect(calls[0].body).toEqual({ spawnId: "a" });
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

describe("setSpawnModel", () => {
  it("POSTs SetSpawnModel with spawnId + model and returns the result", async () => {
    const calls = mockFetch({ model: "openai/gpt-4o", applied: false });
    const res = await setSpawnModel("s1", "openai/gpt-4o");
    expect(calls[0].url).toContain("/cp.v1.SpawnService/SetSpawnModel");
    expect(calls[0].body).toEqual({ spawnId: "s1", model: "openai/gpt-4o" });
    expect(res).toEqual({ model: "openai/gpt-4o", applied: false });
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
