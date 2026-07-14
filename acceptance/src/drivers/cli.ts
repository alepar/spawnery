/**
 * cliDriver: a spawnctl subprocess wrapper implementing the full SpawnDriver interface.
 * spawnctl currently lacks rename/stop/delete — those are STUBS that FAIL (never skip),
 * surfacing the CLI parity gap as visible red (design §Coverage / §Dual-surface).
 */

import { execFile as execFileCb } from "node:child_process";
import type { CreateSpawnOpts, DriverCtx, ForkOpts, SpawnDriver, SpawnId, SpawnStatus } from "./types";
import type { Identity } from "../fixtures/identity-pool";

/** execFileP: a minimal Promise wrapper around child_process.execFile (easy to mock in tests). */
function execFileP(bin: string, args: string[], configHome?: string): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const callback = (err: Error | null, stdout: string | Buffer, stderr: string | Buffer) => {
      if (err) {
        reject(err);
        return;
      }
      resolve({ stdout: stdout.toString(), stderr: stderr.toString() });
    };
    if (configHome) {
      execFileCb(bin, args, { env: { ...process.env, XDG_CONFIG_HOME: configHome } }, callback);
    } else {
      execFileCb(bin, args, callback);
    }
  });
}

/**
 * execFileWithCode: like execFileP, but a non-zero exit is a RESULT, not a rejection — `spawnctl
 * exec` propagates the inner command's exit code via its own process exit code (execcmd.go /
 * terminalcmd.go's runExec), and Node's execFile reports that as an `Error` carrying a numeric
 * `.code`. Only a transport/spawn failure (no numeric code, e.g. ENOENT — spawnctl itself
 * couldn't run) is a genuine rejection.
 */
function execFileWithCode(bin: string, args: string[], configHome?: string): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const callback = (err: Error | null, stdout: string | Buffer, stderr: string | Buffer) => {
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
    };
    if (configHome) {
      execFileCb(bin, args, { env: { ...process.env, XDG_CONFIG_HOME: configHome } }, callback);
    } else {
      execFileCb(bin, args, callback);
    }
  });
}

export interface CliTrustConfig {
  rootCAPath: string;
  trustDomain: string;
  crlStatePath: string;
  crlIssuerPaths: string[];
  crlPaths: string[];
}

export interface CliConfig {
  cpEndpoint: string;
  spawnctlBin: string;
  /** Undefined preserves the dev-token behavior; [] selects stored spawnctl login state. */
  authArgs?: string[];
  /** Isolated XDG_CONFIG_HOME containing spawnctl's real auth.json. */
  configHome?: string;
  trust?: CliTrustConfig;
}

/**
 * buildExecArgs builds the argv for authenticated `spawnctl exec` through the CP relay.
 * Public trust inputs let spawnctl verify the target node before signing the exec-open intent.
 * `--` terminates flag parsing so the inner command's own flags aren't swallowed by spawnctl.
 */
export function buildExecArgs(cfg: CliConfig, identity: Identity, id: SpawnId, cmd: string[]): string[] {
  return [
    "exec",
    "-spawn",
    id,
    "-cp",
    cfg.cpEndpoint,
    ...authArgs(cfg, identity),
    ...trustArgs(cfg),
    "--",
    ...cmd,
  ];
}

function authArgs(cfg: CliConfig, identity: Identity): string[] {
  return cfg.authArgs ?? ["-token", identity.token];
}

function trustArgs(cfg: CliConfig): string[] {
  if (!cfg.trust) return [];
  return [
    "--root-ca", cfg.trust.rootCAPath,
    "--trust-domain", cfg.trust.trustDomain,
    "--crl-state", cfg.trust.crlStatePath,
    ...cfg.trust.crlIssuerPaths.flatMap((path) => ["--crl-issuer", path]),
    ...cfg.trust.crlPaths.flatMap((path) => ["--crl", path]),
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
  const authorization = subcmd === "" || subcmd === "resume" || subcmd === "fork" || subcmd === "move"
    ? trustArgs(cfg)
    : [];
  if (subcmd === "") {
    return ["-cp", cfg.cpEndpoint, ...authArgs(cfg, identity), ...authorization, ...args];
  }
  return [subcmd, ...args, "-cp", cfg.cpEndpoint, ...authArgs(cfg, identity), ...authorization];
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

  /** Shares the fixture-prepared stored-login and trust configuration with auxiliary CLI drivers. */
  configuration(): CliConfig {
    return this.cfg;
  }

  private async run(identity: Identity, subcmd: string, args: string[]): Promise<{ stdout: string; stderr: string }> {
    return execFileP(this.cfg.spawnctlBin, buildArgs(this.cfg, identity, subcmd, args), this.cfg.configHome);
  }

  async createSpawn(ctx: DriverCtx, opts: CreateSpawnOpts): Promise<SpawnId> {
    // -detach makes create a plain, scriptable subprocess: spawnctl creates the spawn, waits for
    // ACTIVE, prints "spawn: <id>", and exits 0 WITHOUT attaching. Crucially, the spawn PERSISTS —
    // a normal attached create issues StopSpawn(id) when its interactive loop ends (stdin EOF), so
    // without -detach the spawn is torn down the moment this subprocess exits.
    const extra = ["-app-id", opts.appId, "-detach"];
    if (opts.model) extra.push("-model", opts.model);
    if (opts.profileId) extra.push("-profile", opts.profileId);
    for (const m of opts.mounts ?? []) {
      // spawnctl --mount name=backend_uri[,create] (mountflag.go). CP derives the gh: credential.
      extra.push("-mount", `${m.name}=${m.backendUri}${m.create ? ",create" : ""}`);
    }
    const args = buildArgs(this.cfg, ctx.identity, "", extra);
    const { stdout } = await execFileP(this.cfg.spawnctlBin, args, this.cfg.configHome);
    const m = /^spawn:\s*(\S+)/m.exec(stdout);
    if (!m) throw new Error(`cliDriver: could not parse spawn id from create output:\n${stdout}`);
    return m[1];
  }

  async rename(_ctx: DriverCtx, _id: SpawnId, _name: string): Promise<void> {
    throw parityGap("rename");
  }

  async setModel(ctx: DriverCtx, id: SpawnId, model: string): Promise<void> {
    await this.run(ctx.identity, "set-model", [id, model]);
  }

  async suspend(ctx: DriverCtx, id: SpawnId): Promise<void> {
    await this.run(ctx.identity, "suspend", [id]);
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

  async move(ctx: DriverCtx, id: SpawnId, targetNodeId: string): Promise<void> {
    await this.run(ctx.identity, "move", [id, targetNodeId]);
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
    const { stdout } = await this.listOutput(ctx);
    return parseListTable(stdout);
  }

  /** Returns spawnctl list's unmodified streams when a caller needs auditable CLI evidence. */
  async listOutput(ctx: DriverCtx): Promise<{ stdout: string; stderr: string }> {
    return this.run(ctx.identity, "list", []);
  }

  /**
   * exec runs `cmd` non-interactively in the spawn's agent container via `spawnctl exec` and
   * returns its exit code + captured stdout/stderr — a NON-throwing result even for a non-zero
   * exit (that's the thing under test: exit-code propagation). CliDriver-only: web has no exec
   * surface, so this is not part of the SpawnDriver interface.
   */
  async exec(ctx: DriverCtx, id: SpawnId, cmd: string[]): Promise<{ code: number; stdout: string; stderr: string }> {
    return execFileWithCode(this.cfg.spawnctlBin, buildExecArgs(this.cfg, ctx.identity, id, cmd), this.cfg.configHome);
  }
}
