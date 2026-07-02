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

describe("ApiDriver token source", () => {
  it("accepts an async token-provider function and calls it fresh per request", async () => {
    const fetchMock = vi.fn().mockImplementation(() => new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const tokens = ["first", "second"];
    const getToken = vi.fn(async () => tokens.shift()!);

    const driver = new ApiDriver("https://cp.example", getToken);
    await driver.listSpawns();
    await driver.listSpawns();

    expect(getToken).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][1].headers["Authorization"]).toBe("Bearer first");
    expect(fetchMock.mock.calls[1][1].headers["Authorization"]).toBe("Bearer second");
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

describe("ApiDriver profiles", () => {
  it("listProfiles posts to ListProfiles and parses {profiles}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ profiles: [{ profileId: "p1", name: "prof", version: 1, updatedAt: "100" }] }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const profiles = await driver.listProfiles();
    expect(fetchMock.mock.calls[0][0]).toBe("https://cp.example/cp.v1.SpawnService/ListProfiles");
    expect(profiles).toEqual([{ profileId: "p1", name: "prof", version: 1, updatedAt: "100" }]);
  });

  it("listProfiles returns an empty array when profiles is absent", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    expect(await driver.listProfiles()).toEqual([]);
  });

  it("getProfile posts profileId to GetProfile and parses {profile}", async () => {
    const profile = { profileId: "p1", name: "prof", version: 2, updatedAt: "100", entries: [], secretIds: [] };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ profile }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const got = await driver.getProfile("p1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/GetProfile");
    expect(JSON.parse(init.body)).toEqual({ profileId: "p1" });
    expect(got).toEqual(profile);
  });

  it("getProfile normalizes omitted entries/secretIds to [] (Connect-JSON omits empty repeated fields)", async () => {
    const profile = { profileId: "p1", name: "prof", version: 1, updatedAt: "100" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ profile }), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    const got = await driver.getProfile("p1");
    expect(got.entries).toEqual([]);
    expect(got.secretIds).toEqual([]);
  });
});

describe("ApiDriver catalog entries", () => {
  it("listCatalogEntries posts to ListCatalogEntries and parses {entries}", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ entries: [{ catalogId: "c1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "sk", description: "" }] }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const entries = await driver.listCatalogEntries();
    expect(fetchMock.mock.calls[0][0]).toBe("https://cp.example/cp.v1.SpawnService/ListCatalogEntries");
    expect(entries).toEqual([{ catalogId: "c1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "sk", description: "" }]);
  });

  it("listCatalogEntries returns an empty array when entries is absent", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    expect(await driver.listCatalogEntries()).toEqual([]);
  });

  it("getCatalogEntry posts catalogId to GetCatalogEntry and parses {entry}", async () => {
    const entry = {
      catalogId: "c1",
      creatorId: "acc-owner-1",
      kind: "PROFILE_ENTRY_KIND_SKILL",
      name: "sk",
      description: "",
      listed: false,
      createdAt: "100",
      updatedAt: "100",
    };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ entry }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const got = await driver.getCatalogEntry("c1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/GetCatalogEntry");
    expect(JSON.parse(init.body)).toEqual({ catalogId: "c1" });
    expect(got).toEqual(entry);
  });

  it("getCatalogEntry normalizes an omitted listed to false (Connect-JSON omits a false bool)", async () => {
    const entry = { catalogId: "c1", creatorId: "acc-owner-1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "sk", description: "" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ entry }), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    const got = await driver.getCatalogEntry("c1");
    expect(got.listed).toBe(false);
  });
});

describe("ApiDriver secrets (oracle-only: no spawnctl secret subcommand)", () => {
  const write = {
    secretId: "s1",
    type: "USER_SECRET_TYPE_GENERIC_KV",
    name: "my secret",
    targetContainer: "ARTIFACT_TARGET_AGENT",
    envVarName: "ACC_SECRET_1",
    devicesetEpoch: 0,
    envelope: "eyJhdF9yZXN0Ijp7fX0=",
  };

  it("createSecret posts {secret: write} to CreateSecret with the bearer header and parses {secret}", async () => {
    const detail = { ...write, version: 1, updatedAt: "100", createdAt: "100" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ secret: detail }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok123");
    const got = await driver.createSecret(write);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/CreateSecret");
    expect(init.headers["Authorization"]).toBe("Bearer tok123");
    expect(JSON.parse(init.body)).toEqual({ secret: write });
    expect(got).toEqual(detail);
  });

  it("listSecrets posts to ListSecrets and parses {secrets}", async () => {
    const summary = { secretId: "s1", type: "USER_SECRET_TYPE_GENERIC_KV", name: "n", targetContainer: "ARTIFACT_TARGET_AGENT", version: 1, devicesetEpoch: 0, updatedAt: "100" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ secrets: [summary] }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const secrets = await driver.listSecrets();
    expect(fetchMock.mock.calls[0][0]).toBe("https://cp.example/cp.v1.SpawnService/ListSecrets");
    expect(secrets).toEqual([summary]);
  });

  it("listSecrets returns an empty array when secrets is absent", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    expect(await driver.listSecrets()).toEqual([]);
  });

  it("getSecret posts secretId to GetSecret and parses {secret}", async () => {
    const detail = { ...write, version: 1, updatedAt: "100", createdAt: "100" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ secret: detail }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    const got = await driver.getSecret("s1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/GetSecret");
    expect(JSON.parse(init.body)).toEqual({ secretId: "s1" });
    expect(got).toEqual(detail);
  });

  it("deleteSecret posts secretId to DeleteSecret", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const driver = new ApiDriver("https://cp.example", "tok");
    await driver.deleteSecret("s1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("https://cp.example/cp.v1.SpawnService/DeleteSecret");
    expect(JSON.parse(init.body)).toEqual({ secretId: "s1" });
  });

  it("surfaces a non-2xx as a thrown error naming the method (AAD/validation failures included)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("invalid envelope", { status: 400 })));
    const driver = new ApiDriver("https://cp.example", "tok");
    await expect(driver.createSecret(write)).rejects.toThrow(/CreateSecret failed: 400/);
  });
});
