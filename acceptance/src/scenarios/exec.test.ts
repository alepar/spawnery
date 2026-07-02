import { describe, it, expect, vi, beforeEach } from "vitest";
import { buildExecArgv, resolveNodeAddr, execConfigFromTarget, type ExecConfig } from "./exec";
import type { TargetConfig } from "../config/target";

const execFileMock = vi.fn();

vi.mock("node:child_process", () => ({
  execFile: (...args: unknown[]) => execFileMock(...args),
}));

describe("buildExecArgv", () => {
  const cfg: ExecConfig = { spawnctlBin: "spawnctl", nodeAddr: "http://n:9092" };

  it("shapes argv as exec -addr <addr> -spawn <id> -- <cmd...>", () => {
    expect(buildExecArgv(cfg, "sp-1", ["cat", "/f"])).toEqual(["exec", "-addr", "http://n:9092", "-spawn", "sp-1", "--", "cat", "/f"]);
  });

  it("leaves a leading-dash cmd token untouched after the -- separator", () => {
    expect(buildExecArgv(cfg, "sp-1", ["sh", "-c", "echo x"])).toEqual([
      "exec",
      "-addr",
      "http://n:9092",
      "-spawn",
      "sp-1",
      "--",
      "sh",
      "-c",
      "echo x",
    ]);
  });
});

describe("resolveNodeAddr", () => {
  it("prefers ACC_NODE_TERMINAL_ADDR when set", () => {
    expect(resolveNodeAddr({ ACC_NODE_TERMINAL_ADDR: "http://z:1" })).toBe("http://z:1");
  });

  it("defaults to http://127.0.0.1:9092 when unset", () => {
    expect(resolveNodeAddr({})).toBe("http://127.0.0.1:9092");
  });
});

describe("execConfigFromTarget", () => {
  it("builds an ExecConfig from the target's spawnctlBin + resolveNodeAddr", () => {
    const target = { spawnctlBin: "../bin/spawnctl" } as TargetConfig;
    expect(execConfigFromTarget(target, { ACC_NODE_TERMINAL_ADDR: "http://z:1" })).toEqual({
      spawnctlBin: "../bin/spawnctl",
      nodeAddr: "http://z:1",
    });
  });
});

describe("execInSpawn / execOrThrow", () => {
  const cfg: ExecConfig = { spawnctlBin: "spawnctl", nodeAddr: "http://n:9092" };

  beforeEach(() => {
    execFileMock.mockReset();
  });

  it("resolves with the exit code on a non-zero command exit (never rejects)", async () => {
    const { execInSpawn } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, cb) => {
      cb(Object.assign(new Error("exit 3"), { code: 3 }), "out", "err");
    });
    await expect(execInSpawn(cfg, "sp-1", ["false"])).resolves.toEqual({ stdout: "out", stderr: "err", code: 3 });
  });

  it("resolves with code 0 on success", async () => {
    const { execInSpawn } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, cb) => {
      cb(null, "ok", "");
    });
    await expect(execInSpawn(cfg, "sp-1", ["true"])).resolves.toEqual({ stdout: "ok", stderr: "", code: 0 });
  });

  it("rejects (does not resolve with a fake code) on a launch failure, naming the binary", async () => {
    const { execInSpawn } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, cb) => {
      cb(Object.assign(new Error("spawn ENOENT"), { code: "ENOENT" }), "", "");
    });
    await expect(execInSpawn(cfg, "sp-1", ["true"])).rejects.toThrow(/spawnctl/);
  });

  it("execOrThrow throws (including stderr) when the command exits non-zero", async () => {
    const { execOrThrow } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, cb) => {
      cb(Object.assign(new Error("exit 1"), { code: 1 }), "", "boom");
    });
    await expect(execOrThrow(cfg, "sp-1", ["false"])).rejects.toThrow(/boom/);
  });

  it("execOrThrow returns the result when the command exits zero", async () => {
    const { execOrThrow } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, cb) => {
      cb(null, "ok", "");
    });
    await expect(execOrThrow(cfg, "sp-1", ["true"])).resolves.toEqual({ stdout: "ok", stderr: "", code: 0 });
  });
});
