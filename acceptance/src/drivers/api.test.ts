import { describe, it, expect, vi, afterEach } from "vitest";
import { ApiDriver } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ApiDriver.listSpawns", () => {
  it("posts Connect-JSON to the right method with the right headers/body and parses {spawns}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          spawns: [
            {
              spawnId: "s1",
              appId: "a1",
              appVersion: "1.0.0",
              model: "m",
              status: "SPAWN_STATUS_ACTIVE",
              createdAt: "100",
              lastUsedAt: "200",
              name: "my-spawn",
              modelApplied: true,
            },
          ],
        }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const driver = new ApiDriver("https://cp.example", "tok123");
    const spawns = await driver.listSpawns();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/ListSpawns");
    expect(init.method).toBe("POST");
    expect(init.headers["Content-Type"]).toBe("application/json");
    expect(init.headers["Connect-Protocol-Version"]).toBe("1");
    expect(init.headers["Authorization"]).toBe("Bearer tok123");
    expect(JSON.parse(init.body)).toEqual({});

    expect(spawns).toEqual([
      {
        spawnId: "s1",
        appId: "a1",
        appVersion: "1.0.0",
        model: "m",
        status: "ACTIVE",
        createdAt: "100",
        lastUsedAt: "200",
        name: "my-spawn",
        modelApplied: true,
        parentSpawnId: undefined,
      },
    ]);
  });

  it("returns an empty array when spawns is absent", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    expect(await driver.listSpawns()).toEqual([]);
  });

  it("surfaces a 401 as a thrown error naming the method", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("unauthorized", { status: 401 })));
    const driver = new ApiDriver("https://cp.example", "bad-token");
    await expect(driver.listSpawns()).rejects.toThrow(/ListSpawns failed: 401/);
  });
});

describe("ApiDriver.findSpawn", () => {
  it("filters ListSpawns by spawnId (no GetSpawn RPC)", async () => {
    // A fresh Response per call — Response bodies can only be read once, and this test calls
    // findSpawn (which calls listSpawns) twice.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(
        () =>
          new Response(
            JSON.stringify({
              spawns: [
                { spawnId: "s1", appId: "a", status: "SPAWN_STATUS_ACTIVE" },
                { spawnId: "s2", appId: "a", status: "SPAWN_STATUS_SUSPENDED" },
              ],
            }),
            { status: 200 },
          ),
      ),
    );
    const driver = new ApiDriver("https://cp.example", "tok");
    const found = await driver.findSpawn("s2");
    expect(found?.status).toBe("SUSPENDED");
    expect(await driver.findSpawn("missing")).toBeUndefined();
  });
});

describe("ApiDriver.createSpawn / deleteSpawn / stopSpawn", () => {
  it("createSpawn posts the request and returns spawnId", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ spawnId: "new-1" }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const id = await driver.createSpawn({ appId: "acc-app" });
    expect(id).toBe("new-1");
    const [url] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/CreateSpawn");
  });

  it("deleteSpawn posts spawnId + destroyData", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    await driver.deleteSpawn("s1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/DeleteSpawn");
    expect(JSON.parse(init.body)).toEqual({ spawnId: "s1", destroyData: true });
  });

  it("stopSpawn posts spawnId", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    await driver.stopSpawn("s1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/StopSpawn");
    expect(JSON.parse(init.body)).toEqual({ spawnId: "s1" });
  });
});
