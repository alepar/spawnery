/**
 * cliDriver: a spawnctl subprocess wrapper implementing the full SpawnDriver interface.
 * spawnctl currently lacks rename/suspend/stop/delete — those are STUBS that FAIL (never skip),
 * surfacing the CLI parity gap as visible red (design §Coverage / §Dual-surface).
 */

import { execFile as execFileCb, spawn as spawnChild } from "node:child_process";
import type { CreateSpawnOpts, DriverCtx, ForkOpts, SpawnDriver, SpawnId, SpawnStatus } from "./types";
import type { Identity } from "../fixtures/identity-pool";

/** execFileP: a minimal Promise wrapper around child_process.execFile (easy to mock in tests). */
function execFileP(bin: string, args: string[]): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFileCb(bin, args, (err, stdout, stderr) => {
      if (err) {
        reject(err);
        return;
      }
      resolve({ stdout: stdout.toString(), stderr: stderr.toString() });
    });
  });
}

/**
 * execFileWithCode: like execFileP, but a non-zero exit is a RESULT, not a rejection — `spawnctl
 * exec` propagates the inner command's exit code via its own process exit code (execcmd.go /
 * terminalcmd.go's runExec), and Node's execFile reports that as an `Error` carrying a numeric
 * `.code`. Only a transport/spawn failure (no numeric code, e.g. ENOENT — spawnctl itself
 * couldn't run) is a genuine rejection.
 */
function execFileWithCode(bin: string, args: string[]): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFileCb(bin, args, (err, stdout, stderr) => {
      if (err) {
        const code = (err as NodeJS.ErrnoException & { code?: unknown }).code;
        if (typeof code === "number") {
          resolve({ code, stdout: stdout?.toString() ?? "", stderr: stderr?.toString() ?? "" });
          return;
        }
        reject(err);
        return;
      }
      resolve({ code: 0, stdout: stdout.toString(), stderr: stderr.toString() });
    });
  });
}

export interface CliConfig {
  cpEndpoint: string;
  spawnctlBin: string;
  /** Node terminal endpoint for `spawnctl exec` (-addr) — optional because most CliDriver verbs
   * (create/list/set-model/...) dial the CP, not the node; only exec() needs it. Defaults to the
   * co-located node terminal endpoint, mirroring TargetConfig.nodeAddr's default. */
  nodeAddr?: string;
}

const DEFAULT_NODE_ADDR = "http://127.0.0.1:9092";

/**
 * buildExecArgs builds the argv for `spawnctl exec`, which dials the NODE directly (-addr), not
 * the CP — the mosh/exec data plane bypasses the control plane entirely (execcmd.go/
 * terminalcmd.go). -cp/-token are exec's own local flags (used only if it ever needs to resolve a
 * spawn interactively) and, per buildArgs' convention, are placed after the subcommand name.
 * `--` terminates flag parsing so the inner command's own flags aren't swallowed by spawnctl.
 */
export function buildExecArgs(cfg: CliConfig, identity: Identity, id: SpawnId, cmd: string[]): string[] {
  return [
    "exec",
    "-spawn",
    id,
    "-addr",
    cfg.nodeAddr ?? DEFAULT_NODE_ADDR,
    "-cp",
    cfg.cpEndpoint,
    "-token",
    identity.token,
    "--",
    ...cmd,
  ];
}

/**
 * buildArgs builds the argv (excluding the binary itself) for one spawnctl invocation.
 *
 * `subcmd === ""` selects the no-subcommand root action (spawn create): -cp/-token are the
 * ROOT's own flags there, so they lead.
 *
 * For every named subcommand (list/set-model/resume/fork/status/...), -cp/-token are NOT
 * persistent/global in the urfave/cli v3 sense — each subcommand redeclares its own local
 * -cp/-token flags with their own defaults (cmd/spawnctl/{list,setmodel,resume,fork,status}.go).
 * Verified empirically against the built binary: `spawnctl -cp X list` still dials the default
 * 127.0.0.1:8080, not X — a value set before the subcommand name is silently ignored. So
 * -cp/-token must be placed AFTER the subcommand name (interspersed with positional args is
 * fine; urfave/cli v3 accepts flags anywhere following the subcommand token).
 */
export function buildArgs(cfg: CliConfig, identity: Identity, subcmd: string, args: string[]): string[] {
  if (subcmd === "") {
    return ["-cp", cfg.cpEndpoint, "-token", identity.token, ...args];
  }
  return [subcmd, ...args, "-cp", cfg.cpEndpoint, "-token", identity.token];
}

function parityGap(verb: string): Error {
  return new Error(`cliDriver: spawnctl has no ${verb} (product parity gap sp-tq0t)`);
}

/** parseListTable parses `spawnctl list`'s tabwriter-aligned stdout table. */
export function parseListTable(stdout: string): { spawnId: string; status: SpawnStatus; name: string }[] {
  const lines = stdout
    .split("\n")
    .map((l) => l.trimEnd())
    .filter((l) => l.length > 0);
  if (lines.length === 0) return []; // `list` prints "no spawns" to stderr and leaves stdout empty
  const rows = lines.slice(1); // first line is the "SPAWN ID  STATUS  NAME  APP" header
  return rows.map((line) => {
    const cols = line.split(/ {2,}/);
    return {
      spawnId: cols[0] ?? "",
      status: (cols[1] ?? "UNSPECIFIED") as SpawnStatus,
      name: cols[2] ?? "",
    };
  });
}

export class CliDriver implements SpawnDriver {
  readonly name = "cli" as const;

  constructor(private readonly cfg: CliConfig) {}

  private async run(identity: Identity, subcmd: string, args: string[]): Promise<{ stdout: string; stderr: string }> {
    return execFileP(this.cfg.spawnctlBin, buildArgs(this.cfg, identity, subcmd, args));
  }

  async createSpawn(ctx: DriverCtx, opts: CreateSpawnOpts): Promise<SpawnId> {
    const extra = ["-app-id", opts.appId];
    if (opts.model) extra.push("-model", opts.model);
    if (opts.profileId) extra.push("-profile", opts.profileId);
    const args = buildArgs(this.cfg, ctx.identity, "", extra);
    // spawnctl's root-action create ATTACHES (interactive driveFrames loop). We must NOT execFile
    // it (that leaves stdin open → it blocks on prompts forever). Instead run it with a CLOSED
    // stdin: it provisions the spawn to ACTIVE, prints "ready. type prompts:", reads EOF, and exits
    // cleanly (exit 0). The spawn keeps running server-side; we detach by letting the client exit.
    const timeoutMs = Number(process.env.ACC_SPAWN_ACTIVE_TIMEOUT_MS) || 240_000;
    return await new Promise<SpawnId>((resolve, reject) => {
      // stdin is an OPEN pipe we never write to: spawnctl provisions the spawn all the way to
      // ACTIVE and prints "ready. type prompts:" (it only reads stdin AFTER that). We watch stdout,
      // and once ACTIVE we end() stdin → spawnctl's prompt-read hits EOF and it exits cleanly, the
      // spawn staying alive server-side. (A closed stdin from the start makes it exit BEFORE
      // provisioning — the spawn never persists.)
      const child = spawnChild(this.cfg.spawnctlBin, args, { stdio: ["pipe", "pipe", "pipe"] });
      let out = "";
      let settled = false;
      const finish = (fn: () => void): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        try {
          child.stdin?.end();
        } catch {
          /* already closed */
        }
        fn();
      };
      const timer = setTimeout(() => {
        child.kill("SIGKILL");
        finish(() => reject(new Error(`cliDriver createSpawn timed out after ${timeoutMs}ms:\n${out}`)));
      }, timeoutMs);
      const onData = (d: Buffer): void => {
        out += d.toString();
        if (/ERROR:|failed at/i.test(out)) {
          child.kill("SIGTERM");
          finish(() => reject(new Error(`cliDriver createSpawn: provisioning failed:\n${out}`)));
          return;
        }
        if (/ready\.\s*type prompts:/i.test(out)) {
          const m = /^spawn:\s*(\S+)/m.exec(out);
          if (m) finish(() => resolve(m[1])); // ACTIVE; finish() ends stdin so spawnctl exits, spawn persists
        }
      };
      child.stdout.on("data", onData);
      child.stderr.on("data", onData);
      child.on("error", (e) => finish(() => reject(e)));
      child.on("close", () => {
        // Fallback: process exited before we saw "ready" (e.g. a non-attaching build).
        const m = /^spawn:\s*(\S+)/m.exec(out);
        if (m) finish(() => resolve(m[1]));
        else finish(() => reject(new Error(`cliDriver: could not parse spawn id from create output:\n${out}`)));
      });
    });
  }

  async rename(_ctx: DriverCtx, _id: SpawnId, _name: string): Promise<void> {
    throw parityGap("rename");
  }

  async setModel(ctx: DriverCtx, id: SpawnId, model: string): Promise<void> {
    await this.run(ctx.identity, "set-model", [id, model]);
  }

  async suspend(_ctx: DriverCtx, _id: SpawnId): Promise<void> {
    throw parityGap("suspend");
  }

  async resume(ctx: DriverCtx, id: SpawnId): Promise<void> {
    await this.run(ctx.identity, "resume", [id]);
  }

  async fork(ctx: DriverCtx, id: SpawnId, opts: ForkOpts): Promise<SpawnId> {
    const args = [id];
    if (opts.targetNodeId) args.push("--node", opts.targetNodeId);
    if (opts.targetClass) args.push("--class", opts.targetClass);
    if (opts.name) args.push("--name", opts.name);
    const { stdout } = await this.run(ctx.identity, "fork", args);
    const m = /fork (\S+) active on node/.exec(stdout);
    if (!m) throw new Error(`cliDriver: could not parse fork spawn id from output:\n${stdout}`);
    return m[1];
  }

  async stop(_ctx: DriverCtx, _id: SpawnId): Promise<void> {
    throw parityGap("stop");
  }

  async delete(_ctx: DriverCtx, _id: SpawnId): Promise<void> {
    throw parityGap("delete");
  }

  /** waitActive polls `spawnctl status <id>` (a single-spawn, single-line surface) rather than the full table. */
  async waitActive(ctx: DriverCtx, id: SpawnId, opts: { timeoutMs?: number; pollMs?: number } = {}): Promise<void> {
    const timeoutMs = opts.timeoutMs ?? 60_000;
    const pollMs = opts.pollMs ?? 1000;
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const { stdout } = await this.run(ctx.identity, "status", [id]);
      const m = /^status:\s*(\S+)/m.exec(stdout);
      const status = m?.[1];
      if (status === "ACTIVE") return;
      if (status === "ERROR" || status === "DELETED") {
        throw new Error(`cliDriver: spawn ${id} reached terminal status ${status} while waiting for ACTIVE`);
      }
      if (Date.now() > deadline) {
        throw new Error(`cliDriver: timed out waiting for spawn ${id} to become ACTIVE (last status ${status ?? "unknown"})`);
      }
      await new Promise((resolve) => setTimeout(resolve, pollMs));
    }
  }

  async list(ctx: DriverCtx): Promise<{ spawnId: SpawnId; status: SpawnStatus; name: string }[]> {
    const { stdout } = await this.run(ctx.identity, "list", []);
    return parseListTable(stdout);
  }

  /**
   * exec runs `cmd` non-interactively in the spawn's agent container via `spawnctl exec` and
   * returns its exit code + captured stdout/stderr — a NON-throwing result even for a non-zero
   * exit (that's the thing under test: exit-code propagation). CliDriver-only: web has no exec
   * surface, so this is not part of the SpawnDriver interface.
   */
  async exec(ctx: DriverCtx, id: SpawnId, cmd: string[]): Promise<{ code: number; stdout: string; stderr: string }> {
    return execFileWithCode(this.cfg.spawnctlBin, buildExecArgs(this.cfg, ctx.identity, id, cmd));
  }
}
