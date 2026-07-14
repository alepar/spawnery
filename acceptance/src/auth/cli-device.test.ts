import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { describe, expect, it, vi } from "vitest";
import { initializeCliOwnerDevice, runCliDeviceLogin } from "./cli-device";

function fakeChild() {
  const child = new EventEmitter() as EventEmitter & {
    stdout: PassThrough;
    stderr: PassThrough;
    kill: ReturnType<typeof vi.fn>;
  };
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.kill = vi.fn();
  return child;
}

describe("runCliDeviceLogin", () => {
  it("approves the emitted user code with the authenticated page and requires a 0600 auth.json", async () => {
    const child = fakeChild();
    const spawn = vi.fn(() => child);
    const post = vi.fn(async () => ({ ok: () => true, text: async () => "authorized" }));
    const stat = vi.fn(async () => ({ mode: 0o100600 }));
    const run = runCliDeviceLogin({
      spawnctlBin: "/fresh/spawnctl",
      asOrigin: "https://vm.example",
      configHome: "/run/worker-0",
      page: { request: { post } },
      timeoutMs: 1_000,
    }, { spawn: spawn as never, stat: stat as never });

    child.stdout.write("To authorize this device, visit:\n  https://vm.example/device/verify\n");
    child.stdout.write("And enter code: BCDF-GHJK\n");
    await new Promise((resolve) => setTimeout(resolve, 0));
    child.emit("exit", 0, null);

    await expect(run).resolves.toBeUndefined();
    expect(spawn).toHaveBeenCalledWith("/fresh/spawnctl", [
      "login", "--device", "--as", "https://vm.example", "--config-dir", "/run/worker-0",
    ], expect.objectContaining({
      env: expect.objectContaining({ XDG_CONFIG_HOME: "/run/worker-0" }),
      stdio: ["ignore", "pipe", "pipe"],
    }));
    expect(post).toHaveBeenCalledWith("https://vm.example/device/verify", {
      form: { user_code: "BCDF-GHJK" },
    });
    expect(stat).toHaveBeenCalledWith("/run/worker-0/auth.json");
  });

  it("fails loudly when the CLI exits before emitting a user code", async () => {
    const child = fakeChild();
    const run = runCliDeviceLogin({
      spawnctlBin: "spawnctl", asOrigin: "https://vm.example", configHome: "/run/w",
      page: { request: { post: vi.fn() } }, timeoutMs: 1_000,
    }, { spawn: (() => child) as never, stat: vi.fn() as never });
    child.stderr.write("device/authorize failed\n");
    child.emit("exit", 1, null);
    await expect(run).rejects.toThrow(/before emitting a user code.*device\/authorize failed/s);
  });

  it("fails when stored credential mode is not owner-only", async () => {
    const child = fakeChild();
    const run = runCliDeviceLogin({
      spawnctlBin: "spawnctl", asOrigin: "https://vm.example", configHome: "/run/w",
      page: { request: { post: vi.fn(async () => ({ ok: () => true, text: async () => "" })) } },
      timeoutMs: 1_000,
    }, { spawn: (() => child) as never, stat: (async () => ({ mode: 0o100644 })) as never });
    child.stdout.write("And enter code: BCDF-GHJK\n");
    await new Promise((resolve) => setTimeout(resolve, 0));
    child.emit("exit", 0, null);
    await expect(run).rejects.toThrow(/auth\.json mode 0644.*0600/);
  });
});

describe("initializeCliOwnerDevice", () => {
  it("keeps the printed recovery phrase out of the harness and requires owner-only files", async () => {
    const child = fakeChild();
    const spawn = vi.fn(() => child);
    const stat = vi.fn(async () => ({ mode: 0o100600 }));
    const run = initializeCliOwnerDevice({
      spawnctlBin: "/fresh/spawnctl",
      configHome: "/run/worker-0",
      timeoutMs: 1_000,
    }, { spawn: spawn as never, stat: stat as never });

    child.emit("exit", 0, null);

    await expect(run).resolves.toBeUndefined();
    expect(spawn).toHaveBeenCalledWith("/fresh/spawnctl", [
      "key", "init", "--config-dir", "/run/worker-0",
    ], expect.objectContaining({
      env: expect.objectContaining({ XDG_CONFIG_HOME: "/run/worker-0" }),
      stdio: "ignore",
    }));
    expect(stat).toHaveBeenCalledWith("/run/worker-0/device.key");
    expect(stat).toHaveBeenCalledWith("/run/worker-0/device-set.json");
  });

  it("fails when the owner device key is not private", async () => {
    const child = fakeChild();
    const run = initializeCliOwnerDevice({
      spawnctlBin: "spawnctl", configHome: "/run/w", timeoutMs: 1_000,
    }, {
      spawn: (() => child) as never,
      stat: (async (path: string) => ({ mode: path.endsWith("device.key") ? 0o100644 : 0o100600 })) as never,
    });
    child.emit("exit", 0, null);
    await expect(run).rejects.toThrow(/device\.key mode 0644.*0600/);
  });
});
