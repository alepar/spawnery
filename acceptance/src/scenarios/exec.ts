/**
 * execInSpawn: runs authenticated `spawnctl exec` through the CP relay. Used by scenarios
 * (marker.ts) to observe a spawn's filesystem without exposing a direct node listener.
 */

import { execFile as execFileCb } from "node:child_process";
import { buildExecArgs, type CliConfig } from "../drivers/cli";
import type { Identity } from "../fixtures/identity-pool";

export type ExecConfig = CliConfig;

export interface ExecResult {
  stdout: string;
  stderr: string;
  code: number;
}

/**
 * buildExecArgv: pure argv builder (excludes the binary). urfave/cli v3 requires flags after the
 * subcommand name; `--` terminates flag parsing so `cmd` is passed through untouched.
 */
export function buildExecArgv(cfg: ExecConfig, identity: Identity, spawnId: string, cmd: string[]): string[] {
  return buildExecArgs(cfg, identity, spawnId, cmd);
}

/**
 * execInSpawn: runs `spawnctl exec`, RESOLVING with {stdout,stderr,code} even on a non-zero
 * command exit (so callers can assert exit codes) — rejects only on a launch failure (binary not
 * found, etc.), where child_process.execFile's err.code is non-numeric.
 */
export function execInSpawn(cfg: ExecConfig, identity: Identity, spawnId: string, cmd: string[]): Promise<ExecResult> {
  return new Promise((resolve, reject) => {
    const callback = (err: Error | null, stdout: string | Buffer, stderr: string | Buffer) => {
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
    };
    const argv = buildExecArgv(cfg, identity, spawnId, cmd);
    execFileCb(cfg.spawnctlBin, argv, { env: { ...process.env, XDG_CONFIG_HOME: cfg.configHome } }, callback);
  });
}

/** execOrThrow: execInSpawn + throw a descriptive Error (including stderr) if the command exited non-zero. */
export async function execOrThrow(cfg: ExecConfig, identity: Identity, spawnId: string, cmd: string[]): Promise<ExecResult> {
  const result = await execInSpawn(cfg, identity, spawnId, cmd);
  if (result.code !== 0) {
    throw new Error(`execOrThrow: spawn ${spawnId} command ${JSON.stringify(cmd)} exited ${result.code}: ${result.stderr}`);
  }
  return result;
}
