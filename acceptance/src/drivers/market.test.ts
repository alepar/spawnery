import { describe, it, expect, vi, beforeEach } from "vitest";
import { spawneryYaml, marketCliArgs, parseRegisterOutput, CliMarketDriver, type RegisterSpec } from "./market";
import type { Identity } from "../fixtures/identity-pool";
import type { DriverCtx } from "./types";

vi.mock("node:child_process", () => ({
  execFile: vi.fn(),
}));

vi.mock("node:fs/promises", () => ({
  mkdtemp: vi.fn(),
  writeFile: vi.fn(),
  mkdir: vi.fn(),
  rm: vi.fn(),
}));

const identity: Identity = { token: "tok-123", owner: "acc-owner-1" };
const cfg = { cpEndpoint: "https://cp.example", spawnctlBin: "spawnctl" };
const ctx = { identity, ns: (b: string) => b, api: {} as DriverCtx["api"] };

const spec: RegisterSpec = {
  id: "acc/acc-r1a2b3-4d5f-w0-probe",
  title: "Acc Probe",
  tags: ["acc"],
  version: "1.0.0",
  ref: "acc/acc-r1a2b3-4d5f-w0-probe@ci",
};

describe("spawneryYaml", () => {
  it("contains the manifest keys and round-trips spec.id/title", () => {
    const yaml = spawneryYaml(spec);
    expect(yaml).toContain("apiVersion: spawnery/v1");
    expect(yaml).toContain("kind: App");
    expect(yaml).toContain(`id: ${spec.id}`);
    expect(yaml).toContain(`title: ${JSON.stringify(spec.title)}`);
    expect(yaml).toContain("agents: { support: [any], requiresAcp: [prompt] }");
    expect(yaml).toContain("model: { recommendedDefault: anthropic/claude-3.5-sonnet }");
    expect(yaml).toContain("visibility: open");
  });

  it("defaults to a single main/data/seed mount when spec.mounts is unset", () => {
    const yaml = spawneryYaml(spec);
    expect(yaml).toContain("- name: main");
    expect(yaml).toContain("path: data");
    expect(yaml).toContain("seed: seed");
  });

  it("emits the mounts spec provides instead of the default", () => {
    const yaml = spawneryYaml({ ...spec, mounts: [{ name: "other", path: "otherdata" }] });
    expect(yaml).toContain("- name: other");
    expect(yaml).toContain("path: otherdata");
    expect(yaml).not.toContain("name: main");
  });

  it("includes description and tags when set", () => {
    const yaml = spawneryYaml({ ...spec, description: "a probe app" });
    expect(yaml).toContain(`description: ${JSON.stringify("a probe app")}`);
    expect(yaml).toContain("tags:");
    expect(yaml).toContain("  - acc");
  });
});

describe("marketCliArgs", () => {
  it("shapes the -register invocation", () => {
    expect(marketCliArgs(spec, "/tmp/acc-market-xyz")).toEqual([
      "-register",
      "-app",
      "/tmp/acc-market-xyz",
      "-version",
      "1.0.0",
      "-ref",
      spec.ref,
    ]);
  });
});

describe("parseRegisterOutput", () => {
  it("parses the runRegister stdout line", () => {
    expect(parseRegisterOutput("registered acc/app1@1.0.0 tier=TRUST_TIER_UNVERIFIED\n")).toEqual({
      appId: "acc/app1",
      version: "1.0.0",
    });
  });

  it("returns null for unrecognized output", () => {
    expect(parseRegisterOutput("some other output\n")).toBeNull();
  });
});

describe("CliMarketDriver.register", () => {
  beforeEach(async () => {
    const cp = await import("node:child_process");
    const fs = await import("node:fs/promises");
    vi.mocked(cp.execFile).mockReset();
    vi.mocked(fs.mkdtemp).mockReset().mockResolvedValue("/tmp/acc-market-xyz");
    vi.mocked(fs.writeFile).mockReset().mockResolvedValue(undefined);
    vi.mocked(fs.mkdir).mockReset().mockResolvedValue(undefined);
    vi.mocked(fs.rm).mockReset().mockResolvedValue(undefined);
  });

  it("writes a temp spawneryapp.yml containing spec.id and invokes spawnctl with -register -app -version -ref, -cp/-token leading", async () => {
    const cp = await import("node:child_process");
    const fs = await import("node:fs/promises");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "registered acc/acc-r1a2b3-4d5f-w0-probe@1.0.0 tier=TRUST_TIER_UNVERIFIED\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliMarketDriver(cfg);
    const result = await driver.register(ctx, spec);

    expect(result).toEqual({ appId: "acc/acc-r1a2b3-4d5f-w0-probe", version: "1.0.0" });

    const writeCall = vi.mocked(fs.writeFile).mock.calls[0];
    expect(writeCall[0]).toBe("/tmp/acc-market-xyz/spawneryapp.yml");
    expect(writeCall[1]).toContain(`id: ${spec.id}`);

    const execCall = vi.mocked(cp.execFile).mock.calls[0];
    expect(execCall[0]).toBe("spawnctl");
    expect(execCall[1]).toEqual([
      "-cp",
      "https://cp.example",
      "-token",
      "tok-123",
      "-register",
      "-app",
      "/tmp/acc-market-xyz",
      "-version",
      "1.0.0",
      "-ref",
      spec.ref,
    ]);

    // temp dir cleaned up
    expect(vi.mocked(fs.rm)).toHaveBeenCalledWith("/tmp/acc-market-xyz", { recursive: true, force: true });
  });

  it("throws when the register output can't be parsed", async () => {
    const cp = await import("node:child_process");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(null, "unexpected output\n", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliMarketDriver(cfg);
    await expect(driver.register(ctx, spec)).rejects.toThrow(/could not parse register output/);
  });

  it("cleans up the temp dir even when execFile rejects", async () => {
    const cp = await import("node:child_process");
    const fs = await import("node:fs/promises");
    vi.mocked(cp.execFile).mockImplementation(((_bin: string, _args: string[], cb: (...a: unknown[]) => void) => {
      cb(new Error("boom"), "", "");
    }) as unknown as typeof cp.execFile);

    const driver = new CliMarketDriver(cfg);
    await expect(driver.register(ctx, spec)).rejects.toThrow(/boom/);
    expect(vi.mocked(fs.rm)).toHaveBeenCalledWith("/tmp/acc-market-xyz", { recursive: true, force: true });
  });
});

describe("CliMarketDriver — failing stubs (product parity gap, never skip)", () => {
  const driver = new CliMarketDriver(cfg);

  it("browse throws naming the parity gap", async () => {
    await expect(driver.browse(ctx)).rejects.toThrow(/marketplace: spawnctl has no browse/);
  });
  it("openDetail throws naming the parity gap", async () => {
    await expect(driver.openDetail(ctx, "acc/app1")).rejects.toThrow(/marketplace: spawnctl has no openDetail/);
  });
  it("listMine throws naming the parity gap", async () => {
    await expect(driver.listMine(ctx)).rejects.toThrow(/marketplace: spawnctl has no listMine/);
  });
  it("setListing throws naming the parity gap", async () => {
    await expect(driver.setListing(ctx, "acc/app1", false)).rejects.toThrow(/marketplace: spawnctl has no setListing/);
  });
});
