import { describe, expect, it } from "vitest";
import { nodeAdminFromEnv, runShell, restartNode } from "./nodeadmin";

describe("nodeAdminFromEnv", () => {
  it("throws, naming the missing precondition, when ACC_NODE_RESTART_CMD is unset", () => {
    expect(() => nodeAdminFromEnv({})).toThrow(/ACC_NODE_RESTART_CMD/);
  });

  it("returns the command and the default timeout", () => {
    const cfg = nodeAdminFromEnv({ ACC_NODE_RESTART_CMD: "ssh vm sudo systemctl restart spawnery-node" });
    expect(cfg.restartCmd).toBe("ssh vm sudo systemctl restart spawnery-node");
    expect(cfg.timeoutMs).toBe(120_000);
  });

  it("honours ACC_NODE_RESTART_TIMEOUT_MS", () => {
    const cfg = nodeAdminFromEnv({ ACC_NODE_RESTART_CMD: "true", ACC_NODE_RESTART_TIMEOUT_MS: "5000" });
    expect(cfg.timeoutMs).toBe(5000);
  });
});

describe("runShell / restartNode", () => {
  it("resolves with the exit code and stdout of the command", async () => {
    const res = await runShell("echo hello", 10_000);
    expect(res.code).toBe(0);
    expect(res.stdout.trim()).toBe("hello");
  });

  it("restartNode throws (with the exit code) when the restart command fails", async () => {
    await expect(restartNode({ restartCmd: "exit 3", timeoutMs: 10_000 })).rejects.toThrow(/exited 3/);
  });

  it("restartNode resolves when the restart command succeeds", async () => {
    await expect(restartNode({ restartCmd: "true", timeoutMs: 10_000 })).resolves.toBeUndefined();
  });
});
