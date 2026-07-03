import { describe, it, expect, vi } from "vitest";
import { SpawnRegistry } from "./spawn-registry";

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
