/**
 * nodeadmin: the ONE operator action this black-box suite is allowed to take against the target's node —
 * restarting the spawnlet (`systemctl restart spawnery-node`, the documented upgrade path,
 * docs/e2e-vm-testing.md). It exists for the SE3 restart scenario
 * (tests/lifecycle/node-restart.spec.ts): a restart must leave every running spawn running.
 *
 * The suite provisions nothing and knows nothing about the target's host, so the action is injected as an
 * opaque HOST shell command via ACC_NODE_RESTART_CMD — on the e2e-VM lane, scripts/e2e-vm/run.sh exports
 * an `ssh … sudo systemctl restart spawnery-node`. Unset => the scenario FAILS loudly naming the missing
 * precondition (a missing dependency is an error, never a silent skip).
 */

import { execFile as execFileCb } from "node:child_process";

export interface NodeAdminConfig {
  /** A host shell command that restarts the target's spawnlet and returns 0 on success. */
  restartCmd: string;
  /** Hard cap on the restart command (ssh + systemctl on a slow VM is not instant). */
  timeoutMs: number;
}

export interface ShellResult {
  stdout: string;
  stderr: string;
  code: number;
}

const DEFAULT_TIMEOUT_MS = 120_000;

/** nodeAdminFromEnv: reads ACC_NODE_RESTART_CMD (+ optional ACC_NODE_RESTART_TIMEOUT_MS), throwing a
 * precondition error that names the var and how the e2e-VM lane provides it. */
export function nodeAdminFromEnv(env: NodeJS.ProcessEnv = process.env): NodeAdminConfig {
  const restartCmd = env.ACC_NODE_RESTART_CMD;
  if (!restartCmd) {
    throw new Error(
      "missing precondition ACC_NODE_RESTART_CMD: the node-restart scenario needs a host shell command " +
        "that restarts the target's spawnlet (e.g. `ssh -i <key> user@<vm> sudo systemctl restart spawnery-node`). " +
        "scripts/e2e-vm/run.sh exports it for the e2e-VM lane; other targets must provide it (see acceptance/.env.example). " +
        "Exclude this scenario with `--grep-invert @noderestart` on targets whose node you cannot restart.",
    );
  }
  const raw = env.ACC_NODE_RESTART_TIMEOUT_MS;
  const timeoutMs = raw ? Number(raw) : DEFAULT_TIMEOUT_MS;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    throw new Error(`ACC_NODE_RESTART_TIMEOUT_MS must be a positive number, got ${JSON.stringify(raw)}`);
  }
  return { restartCmd, timeoutMs };
}

/** runShell: runs cmd through /bin/sh, RESOLVING with {stdout,stderr,code} even on a non-zero exit. */
export function runShell(cmd: string, timeoutMs: number): Promise<ShellResult> {
  return new Promise((resolve, reject) => {
    execFileCb("/bin/sh", ["-c", cmd], { timeout: timeoutMs }, (err, stdout, stderr) => {
      if (err) {
        const code = (err as NodeJS.ErrnoException).code;
        if (typeof code === "number") {
          resolve({ stdout: stdout.toString(), stderr: stderr.toString(), code });
          return;
        }
        reject(new Error(`runShell: ${cmd}: ${err.message}`));
        return;
      }
      resolve({ stdout: stdout.toString(), stderr: stderr.toString(), code: 0 });
    });
  });
}

/** restartNode: runs the configured restart command, throwing (with the exit code + stderr) if it fails. */
export async function restartNode(cfg: NodeAdminConfig): Promise<void> {
  const res = await runShell(cfg.restartCmd, cfg.timeoutMs);
  if (res.code !== 0) {
    throw new Error(`restartNode: ${JSON.stringify(cfg.restartCmd)} exited ${res.code}: ${res.stderr.trim()}`);
  }
}
