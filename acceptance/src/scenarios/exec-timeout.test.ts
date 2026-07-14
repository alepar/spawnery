import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { execInSpawn, type ExecConfig } from "./exec";
import type { Identity } from "../fixtures/identity-pool";

const identity: Identity = { token: "ignored", owner: "alice" };
let testDir: string | undefined;
let childPid: number | undefined;

afterEach(() => {
  if (childPid !== undefined) {
    try {
      process.kill(childPid, "SIGKILL");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
    }
  }
  childPid = undefined;
  if (testDir) rmSync(testDir, { recursive: true, force: true });
  testDir = undefined;
});

describe("execInSpawn timeout", () => {
  it("terminates the spawnctl child and rejects when the native timeout elapses", async () => {
    testDir = mkdtempSync(join(tmpdir(), "acceptance-exec-timeout-"));
    const bin = join(testDir, "slow-spawnctl");
    const pidFile = join(testDir, "pid");
    writeFileSync(bin, `#!/bin/sh\nprintf %s "$$" > "${pidFile}"\nexec sleep 2\n`);
    chmodSync(bin, 0o700);

    const cfg: ExecConfig = {
      cpEndpoint: "https://cp.example",
      spawnctlBin: bin,
      authArgs: [],
    };
    const startedAt = Date.now();
    await expect(execInSpawn(cfg, identity, "sp-1", ["true"], { timeoutMs: 50 })).rejects.toThrow(
      /timed out after 50ms/,
    );
    expect(Date.now() - startedAt).toBeLessThan(1_000);

    const pid = Number(readFileSync(pidFile, "utf8"));
    childPid = pid;
    expect(Number.isInteger(pid)).toBe(true);
    expect(() => process.kill(pid, 0)).toThrow(expect.objectContaining({ code: "ESRCH" }));
  });
});
