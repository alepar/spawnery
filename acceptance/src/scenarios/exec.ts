/**
 * execInSpawn: runs `spawnctl exec` against a spawn's agent container over the node terminal
 * endpoint (cmd/spawnctl/terminalcmd.go). Used by scenarios (marker.ts) to write/read workspace
 * state without a browser — the only way to observe a spawn's filesystem black-box.
 *
 * `-spawn <id>` short-circuits spawnctl's resolveSpawn (it never dials -cp in that case), so
 * this module only needs -addr + -spawn — no cpEndpoint/token here (unlike cli.ts's CliConfig).
 */

import { execFile as execFileCb } from "node:child_process";
import type { TargetConfig } from "../config/target";

export interface ExecConfig {
  spawnctlBin: string;
  nodeAddr: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  code: number;
}

/** resolveNodeAddr: ACC_NODE_TERMINAL_ADDR env → default "http://127.0.0.1:9092" (single-node/co-located target). */
export function resolveNodeAddr(env: NodeJS.ProcessEnv = process.env): string {
  return env.ACC_NODE_TERMINAL_ADDR || "http://127.0.0.1:9092";
}

/** execConfigFromTarget: builds an ExecConfig from the target's spawnctlBin + resolveNodeAddr, keeping the spec thin. */
export function execConfigFromTarget(target: TargetConfig, env: NodeJS.ProcessEnv = process.env): ExecConfig {
  return { spawnctlBin: target.spawnctlBin, nodeAddr: resolveNodeAddr(env) };
}

/**
 * buildExecArgv: pure argv builder (excludes the binary). urfave/cli v3 requires flags after the
 * subcommand name; `--` terminates flag parsing so `cmd` is passed through untouched.
 */
export function buildExecArgv(cfg: ExecConfig, spawnId: string, cmd: string[]): string[] {
  return ["exec", "-addr", cfg.nodeAddr, "-spawn", spawnId, "--", ...cmd];
}

/**
 * execInSpawn: runs `spawnctl exec`, RESOLVING with {stdout,stderr,code} even on a non-zero
 * command exit (so callers can assert exit codes) — rejects only on a launch failure (binary not
 * found, etc.), where child_process.execFile's err.code is non-numeric.
 */
export function execInSpawn(cfg: ExecConfig, spawnId: string, cmd: string[]): Promise<ExecResult> {
  return new Promise((resolve, reject) => {
    execFileCb(cfg.spawnctlBin, buildExecArgv(cfg, spawnId, cmd), (err, stdout, stderr) => {
      if (err) {
        const exitCode = (err as NodeJS.ErrnoException).code;
        if (typeof exitCode === "number") {
          resolve({ stdout: stdout.toString(), stderr: stderr.toString(), code: exitCode });
          return;
        }
        reject(new Error(`execInSpawn: failed to launch ${cfg.spawnctlBin}: ${err.message}`));
        return;
      }
      resolve({ stdout: stdout.toString(), stderr: stderr.toString(), code: 0 });
    });
  });
}

/** execOrThrow: execInSpawn + throw a descriptive Error (including stderr) if the command exited non-zero. */
export async function execOrThrow(cfg: ExecConfig, spawnId: string, cmd: string[]): Promise<ExecResult> {
  const result = await execInSpawn(cfg, spawnId, cmd);
  if (result.code !== 0) {
    throw new Error(`execOrThrow: spawn ${spawnId} command ${JSON.stringify(cmd)} exited ${result.code}: ${result.stderr}`);
  }
  return result;
}
