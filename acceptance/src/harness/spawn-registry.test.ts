import { describe, it, expect, vi } from "vitest";
import { cleanupNewSpawns, SpawnRegistry } from "./spawn-registry";

describe("SpawnRegistry", () => {
  it("track returns the id it was given", () => {
    const deleteSpawn = vi.fn().mockResolvedValue(undefined);
    const reg = new SpawnRegistry({ deleteSpawn });
    expect(reg.track("spawn-1")).toBe("spawn-1");
  });

  it("de-dupes: tracking the same id twice deletes it exactly once", async () => {
    const deleteSpawn = vi.fn().mockResolvedValue(undefined);
    const reg = new SpawnRegistry({ deleteSpawn });
    reg.track("spawn-1");
    reg.track("spawn-1");
    await reg.cleanup();
    expect(deleteSpawn).toHaveBeenCalledTimes(1);
    expect(deleteSpawn).toHaveBeenCalledWith("spawn-1");
  });

  it("cleanup deletes every distinct tracked id", async () => {
    const deleteSpawn = vi.fn().mockResolvedValue(undefined);
    const reg = new SpawnRegistry({ deleteSpawn });
    reg.track("spawn-1");
    reg.track("spawn-2");
    reg.track("spawn-3");
    await reg.cleanup();
    expect(deleteSpawn).toHaveBeenCalledTimes(3);
    expect(deleteSpawn).toHaveBeenCalledWith("spawn-1");
    expect(deleteSpawn).toHaveBeenCalledWith("spawn-2");
    expect(deleteSpawn).toHaveBeenCalledWith("spawn-3");
  });

  it("swallows a rejecting deleteSpawn and still deletes the others", async () => {
    const deleteSpawn = vi.fn().mockImplementation((id: string) => {
      if (id === "spawn-2") return Promise.reject(new Error("boom"));
      return Promise.resolve(undefined);
    });
    const reg = new SpawnRegistry({ deleteSpawn });
    reg.track("spawn-1");
    reg.track("spawn-2");
    reg.track("spawn-3");
    await expect(reg.cleanup()).resolves.toBeUndefined();
    expect(deleteSpawn).toHaveBeenCalledTimes(3);
  });

  it("clears the set after cleanup — a second cleanup is a no-op", async () => {
    const deleteSpawn = vi.fn().mockResolvedValue(undefined);
    const reg = new SpawnRegistry({ deleteSpawn });
    reg.track("spawn-1");
    await reg.cleanup();
    expect(deleteSpawn).toHaveBeenCalledTimes(1);
    deleteSpawn.mockClear();
    await reg.cleanup();
    expect(deleteSpawn).not.toHaveBeenCalled();
  });
});

describe("cleanupNewSpawns", () => {
  it("deletes only owner-visible ids added after the baseline snapshot", async () => {
    const deleteSpawn = vi.fn().mockResolvedValue(undefined);
    const listSpawns = vi.fn().mockResolvedValue([
      { spawnId: "existing" },
      { spawnId: "new-active" },
      { spawnId: "new-error" },
    ]);

    await cleanupNewSpawns({ listSpawns, deleteSpawn }, new Set(["existing"]));

    expect(deleteSpawn.mock.calls.map(([id]) => id)).toEqual(["new-active", "new-error"]);
  });

  it("continues deleting the post-test delta after one delete fails", async () => {
    const deleteSpawn = vi.fn().mockImplementation((id: string) => (
      id === "new-error" ? Promise.reject(new Error("boom")) : Promise.resolve()
    ));
    const listSpawns = vi.fn().mockResolvedValue([
      { spawnId: "new-error" },
      { spawnId: "new-active" },
    ]);

    await expect(cleanupNewSpawns({ listSpawns, deleteSpawn }, new Set())).resolves.toBeUndefined();
    expect(deleteSpawn).toHaveBeenCalledTimes(2);
  });
});
