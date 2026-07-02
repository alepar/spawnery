import { describe, it, expect, vi, beforeEach } from "vitest";
import { buildArgs, buildExecArgs, parseListTable, CliDriver } from "./cli";
import type { Identity } from "../fixtures/identity-pool";
import type { DriverCtx } from "./types";

vi.mock("node:child_process", () => ({
  execFile: vi.fn(),
}));

const identity: Identity = { token: "tok-123", owner: "acc-owner-1" };
const cfg = { cpEndpoint: "https://cp.example", spawnctlBin: "spawnctl", nodeAddr: "http://node.example:9092" };

describe("buildArgs", () => {
  it("puts -cp/-token as leading root flags for the no-subcommand create form", () => {
    const argv = buildArgs(cfg, identity, "", ["-app-id", "acc/app"]);
    expect(argv).toEqual(["-cp", "https://cp.example", "-token", "tok-123", "-app-id", "acc/app"]);
  });

  it("puts -cp/-token AFTER the subcommand name for named subcommands", () => {
    // Verified against the real binary: list/set-model/resume/fork/status each redeclare their
    // OWN local -cp/-token flags; a value set before the subcommand name is silently ignored.
    const argv = buildArgs(cfg, identity, "list", []);
    expect(argv).toEqual(["list", "-cp", "https://cp.example", "-token", "tok-123"]);
  });

  it("keeps positional args before the trailing -cp/-token for a subcommand", () => {
    const argv = buildArgs(cfg, identity, "set-model", ["s1", "some-model"]);
    expect(argv).toEqual(["set-model", "s1", "some-model", "-cp", "https://cp.example", "-token", "tok-123"]);
  });
});

describe("buildExecArgs", () => {
  it("targets the node (-addr), not the CP, with -spawn and a -- terminator before the command", () => {
    const argv = buildExecArgs(cfg, identity, "s1", ["cat", "/workspace/marker.txt"]);
    expect(argv).toEqual([
      "exec",
      "-spawn",
      "s1",
      "-addr",
      "http://node.example:9092",
      "-cp",
      "https://cp.example",
      "-token",
      "tok-123",
      "--",
      "cat",
      "/workspace/marker.txt",
    ]);
  });

  it("places -cp/-token after the subcommand and -- strictly before the inner command", () => {
    const argv = buildExecArgs(cfg, identity, "s1", ["sh", "-c", "exit 3"]);
    const dashDash = argv.indexOf("--");
    expect(dashDash).toBeGreaterThan(argv.indexOf("-spawn"));
    expect(dashDash).toBeGreaterThan(argv.indexOf("-token"));
    expect(argv.slice(dashDash + 1)).toEqual(["sh", "-c", "exit 3"]);
  });
});

describe("parseListTable", () => {
  it("parses a tabwriter-aligned table, skipping the header", () => {
    const stdout = "SPAWN ID  STATUS  NAME    APP\ns1        ACTIVE  my-app  acc/app1\ns2        SUSPENDED  -       acc/app2\n";
    expect(parseListTable(stdout)).toEqual([
      { spawnId: "s1", status: "ACTIVE", name: "my-app" },
      { spawnId: "s2", status: "SUSPENDED", name: "-" },
    ]);
  });

  it("returns an empty array for empty stdout (the no-spawns case writes to stderr instead)", () => {
    expect(parseListTable("")).toEqual([]);
  });
});

describe("CliDriver — failing stubs (product parity gap, never skip)", () => {
  const driver = new CliDriver(cfg);
  const ctx = { identity, ns: (b: string) => b, api: {} as DriverCtx["api"] };

  it("rename throws naming the parity gap", async () => {
    await expect(driver.rename(ctx, "s1", "new-name")).rejects.toThrow(/spawnctl has no rename/);
  });
  it("suspend throws naming the parity gap", async () => {
    await expect(driver.suspend(ctx, "s1")).rejects.toThrow(/spawnctl has no suspend/);
  });
  it("stop throws naming the parity gap", async () => {
    await expect(driver.stop(ctx, "s1")).rejects.toThrow(/spawnctl has no stop/);
  });
  it("delete throws naming the parity gap", async () => {
    await expect(driver.delete(ctx, "s1")).rejects.toThrow(/spawnctl has no delete/);
  });
});

describe("CliDriver — subprocess-backed verbs (execFile mocked)", () => {
  const ctx = { identity, ns: (b: string) => b, api: {} as DriverCtx["api"] };

  beforeEach(async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockReset();
  });

  it("createSpawn parses the spawn id from stdout", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "spawn: acc-spawn-1\nsome other progress line\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    const id = await driver.createSpawn(ctx, { appId: "acc/app" });
    expect(id).toBe("acc-spawn-1");
  });

  it("createSpawn includes -profile <id> in argv when opts.profileId is set", async () => {
    const cp = await import("node:child_process");
    let seenArgs: string[] = [];
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, args: string[], cb: (...a: unknown[]) => void) => {
      seenArgs = args;
      cb(null, "spawn: acc-spawn-1\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await driver.createSpawn(ctx, { appId: "acc/app", profileId: "prof-1" });
    expect(seenArgs).toContain("-profile");
    expect(seenArgs[seenArgs.indexOf("-profile") + 1]).toBe("prof-1");
  });

  it("createSpawn omits -profile when opts.profileId is not set", async () => {
    const cp = await import("node:child_process");
    let seenArgs: string[] = [];
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, args: string[], cb: (...a: unknown[]) => void) => {
      seenArgs = args;
      cb(null, "spawn: acc-spawn-1\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await driver.createSpawn(ctx, { appId: "acc/app" });
    expect(seenArgs).not.toContain("-profile");
  });

  it("createSpawn throws when the spawn id can't be parsed", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "unexpected output\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.createSpawn(ctx, { appId: "acc/app" })).rejects.toThrow(/could not parse spawn id/);
  });

  it("fork parses the fork spawn id from stdout", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "fork s1\n  target same node\n  fork acc-fork-1 active on node n1\n  done.\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    const id = await driver.fork(ctx, "s1", {});
    expect(id).toBe("acc-fork-1");
  });

  it("waitActive returns once status polls ACTIVE", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "status: ACTIVE\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.waitActive(ctx, "s1", { timeoutMs: 5000, pollMs: 1 })).resolves.toBeUndefined();
  });

  it("waitActive throws on a terminal ERROR status", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "status: ERROR\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.waitActive(ctx, "s1", { timeoutMs: 5000, pollMs: 1 })).rejects.toThrow(/terminal status ERROR/);
  });

  it("exec resolves {code:0} on a clean run, without throwing", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "hello\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.exec(ctx, "s1", ["printf", "hello"])).resolves.toEqual({ code: 0, stdout: "hello\n", stderr: "" });
  });

  it("exec resolves a non-zero code (from the propagated inner command exit) without throwing", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(Object.assign(new Error("Command failed"), { code: 3 }), "partial out", "some err");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.exec(ctx, "s1", ["sh", "-c", "exit 3"])).resolves.toEqual({
      code: 3,
      stdout: "partial out",
      stderr: "some err",
    });
  });

  it("exec rejects a transport failure that carries no numeric exit code (e.g. ENOENT)", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(Object.assign(new Error("spawn spawnctl ENOENT"), { code: "ENOENT" }), "", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliDriver(cfg);
    await expect(driver.exec(ctx, "s1", ["true"])).rejects.toThrow(/ENOENT/);
  });
});
