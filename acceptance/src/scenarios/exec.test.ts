import { describe, it, expect, vi, beforeEach } from "vitest";
import { buildExecArgv, type ExecConfig } from "./exec";
import type { Identity } from "../fixtures/identity-pool";

const { execFileMock } = vi.hoisted(() => ({ execFileMock: vi.fn() }));
vi.mock("node:child_process", () => ({ execFile: execFileMock }));

const identity: Identity = { token: "ignored", owner: "alice" };
const cfg: ExecConfig = {
  cpEndpoint: "https://cp.example", spawnctlBin: "spawnctl", authArgs: [], configHome: "/run/cli-auth",
  trust: {
    rootCAPath: "/run/root.pem", trustDomain: "prod.spawnery.internal", crlStatePath: "/run/crl.json",
    crlIssuerPaths: ["/run/issuer.pem"], crlPaths: ["/run/current.crl"],
  },
};

describe("buildExecArgv", () => {
  it("builds authenticated CP-relayed exec argv without a direct node address", () => {
    const argv = buildExecArgv(cfg, identity, "sp-1", ["cat", "/f"]);
    expect(argv).not.toContain("-addr");
    expect(argv).toEqual([
      "exec", "-spawn", "sp-1", "-cp", "https://cp.example",
      "--root-ca", "/run/root.pem", "--trust-domain", "prod.spawnery.internal",
      "--crl-state", "/run/crl.json", "--crl-issuer", "/run/issuer.pem", "--crl", "/run/current.crl",
      "--", "cat", "/f",
    ]);
  });
});

describe("execInSpawn / execOrThrow", () => {
  beforeEach(() => {
    execFileMock.mockReset();
  });

  it("passes isolated stored custody and resolves a non-zero command exit", async () => {
    const { execInSpawn } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, options, cb) => {
      expect(options.env.XDG_CONFIG_HOME).toBe("/run/cli-auth");
      cb(Object.assign(new Error("exit 3"), { code: 3 }), "out", "err");
    });
    await expect(execInSpawn(cfg, identity, "sp-1", ["false"])).resolves.toEqual({ stdout: "out", stderr: "err", code: 3 });
  });

  it("resolves with code 0 on success", async () => {
    const { execInSpawn } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, _options, cb) => cb(null, "ok", ""));
    await expect(execInSpawn(cfg, identity, "sp-1", ["true"])).resolves.toEqual({ stdout: "ok", stderr: "", code: 0 });
  });

  it("execOrThrow includes stderr for a non-zero exit", async () => {
    const { execOrThrow } = await import("./exec");
    execFileMock.mockImplementation((_bin, _args, _options, cb) => cb(Object.assign(new Error("exit 1"), { code: 1 }), "", "boom"));
    await expect(execOrThrow(cfg, identity, "sp-1", ["false"])).rejects.toThrow(/boom/);
  });
});
