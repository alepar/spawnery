/**
 * customization.ts: cli-primary drivers for the customization surface (Phase 5, sp-tq0t.8) —
 * `ProfileCli`/`CatalogCli` (spawnctl `profile`/`catalog` subcommand wrappers), `execInSpawn`
 * (observes profile-attached artifacts inside a spawn via the node's `spawnctl exec`), and two
 * small builders shared by the scenarios: `buildSkillTar` (a minimal custom-skill payload) and
 * `dummyAtRestEnvelope` (a CRUD-only secret envelope — see the note on secrets below).
 *
 * Secrets have NO `spawnctl secret` subcommand (CLI parity gap, sp-tq0t product debt) — secret
 * CRUD goes through the `apiDriver` oracle instead (see drivers/api.ts's secret methods); nothing
 * in this module talks to secrets directly.
 */

import { execFile as execFileCb } from "node:child_process";
import { buildArgs, buildExecArgs, type CliConfig } from "./cli";
import type { Identity } from "../fixtures/identity-pool";

/**
 * execFileP2 resolves with `{stdout, stderr, code}` in ALL cases — a non-zero exit is DATA, not a
 * thrown error, so callers (execInSpawn in particular) can assert on it. Kept module-private
 * (rather than added to cli.ts) because cli.ts's own execFileP intentionally always rejects on a
 * non-zero exit — the two call sites want different failure semantics.
 */
function execFileP2(bin: string, args: string[], configHome?: string): Promise<{ stdout: string; stderr: string; code: number }> {
  return new Promise((resolve) => {
    const callback = (err: Error | null, stdout: string | Buffer, stderr: string | Buffer) => {
      let code = 0;
      if (err) {
        const c = (err as { code?: number | string }).code;
        code = typeof c === "number" ? c : 1;
      }
      resolve({ stdout: stdout.toString(), stderr: stderr.toString(), code });
    };
    if (configHome) {
      execFileCb(bin, args, { env: { ...process.env, XDG_CONFIG_HOME: configHome } }, callback);
    } else {
      execFileCb(bin, args, callback);
    }
  });
}

/** ProfileCli wraps `spawnctl profile` (CRUD, entries, secret refs). CAS versioning is left to the
 * CLI's own read-modify-write (no --version passed) — see cmd/spawnctl/profile.go. */
export class ProfileCli {
  constructor(
    private readonly cfg: CliConfig,
    private readonly identity: Identity,
  ) {}

  private async run(args: string[]): Promise<{ stdout: string; stderr: string }> {
    const argv = buildArgs(this.cfg, this.identity, "profile", args);
    const { stdout, stderr, code } = await execFileP2(this.cfg.spawnctlBin, argv, this.cfg.configHome);
    if (code !== 0) {
      throw new Error(`spawnctl profile ${args.join(" ")} exited ${code}\nstdout:\n${stdout}\nstderr:\n${stderr}`);
    }
    return { stdout, stderr };
  }

  async create(name: string): Promise<string> {
    const { stdout } = await this.run(["create", name]);
    const m = /created profile (\S+)/.exec(stdout);
    if (!m) throw new Error(`ProfileCli.create: could not parse profile id from output:\n${stdout}`);
    return m[1];
  }

  async list(): Promise<string> {
    return (await this.run(["list"])).stdout;
  }

  async show(profileId: string): Promise<string> {
    return (await this.run(["show", profileId])).stdout;
  }

  async rename(profileId: string, newName: string): Promise<void> {
    await this.run(["rename", profileId, newName]);
  }

  async delete(profileId: string): Promise<void> {
    await this.run(["delete", profileId]);
  }

  async entryAddCustom(
    profileId: string,
    opts: { kind: string; name: string; customFilePath: string; targets?: string[] },
  ): Promise<string> {
    const args = ["entry", "add", profileId, "--kind", opts.kind, "--name", opts.name, "--custom-file", opts.customFilePath];
    for (const t of opts.targets ?? []) args.push("--target", t);
    const { stdout } = await this.run(args);
    const m = /added entry (\S+)/.exec(stdout);
    if (!m) throw new Error(`ProfileCli.entryAddCustom: could not parse entry id from output:\n${stdout}`);
    return m[1];
  }

  async entryAddCatalog(profileId: string, opts: { kind: string; name: string; catalogId: string }): Promise<string> {
    const { stdout } = await this.run(["entry", "add", profileId, "--kind", opts.kind, "--name", opts.name, "--catalog", opts.catalogId]);
    const m = /added entry (\S+)/.exec(stdout);
    if (!m) throw new Error(`ProfileCli.entryAddCatalog: could not parse entry id from output:\n${stdout}`);
    return m[1];
  }

  async entryRemove(profileId: string, entryId: string): Promise<void> {
    await this.run(["entry", "remove", profileId, entryId]);
  }

  async secretAdd(profileId: string, secretId: string): Promise<void> {
    await this.run(["secret", "add", profileId, secretId]);
  }

  async secretRemove(profileId: string, secretId: string): Promise<void> {
    await this.run(["secret", "remove", profileId, secretId]);
  }
}

/** CatalogCli wraps `spawnctl catalog` (CRUD + listing toggle). See cmd/spawnctl/catalog.go. */
export class CatalogCli {
  constructor(
    private readonly cfg: CliConfig,
    private readonly identity: Identity,
  ) {}

  private async run(args: string[]): Promise<{ stdout: string; stderr: string }> {
    const argv = buildArgs(this.cfg, this.identity, "catalog", args);
    const { stdout, stderr, code } = await execFileP2(this.cfg.spawnctlBin, argv, this.cfg.configHome);
    if (code !== 0) {
      throw new Error(`spawnctl catalog ${args.join(" ")} exited ${code}\nstdout:\n${stdout}\nstderr:\n${stderr}`);
    }
    return { stdout, stderr };
  }

  async create(opts: { name: string; kind: string; description?: string; contentFilePath?: string }): Promise<string> {
    const args = ["create", opts.name, "--kind", opts.kind];
    if (opts.description !== undefined) args.push("--description", opts.description);
    if (opts.contentFilePath !== undefined) args.push("--content-file", opts.contentFilePath);
    const { stdout } = await this.run(args);
    const m = /created catalog entry (\S+)/.exec(stdout);
    if (!m) throw new Error(`CatalogCli.create: could not parse catalog id from output:\n${stdout}`);
    return m[1];
  }

  async list(): Promise<string> {
    return (await this.run(["list"])).stdout;
  }

  async show(catalogId: string): Promise<string> {
    return (await this.run(["show", catalogId])).stdout;
  }

  async update(catalogId: string, opts: { name?: string; description?: string; contentFilePath?: string }): Promise<void> {
    const args = ["update", catalogId];
    if (opts.name !== undefined) args.push("--name", opts.name);
    if (opts.description !== undefined) args.push("--description", opts.description);
    if (opts.contentFilePath !== undefined) args.push("--content-file", opts.contentFilePath);
    await this.run(args);
  }

  async delete(catalogId: string): Promise<void> {
    await this.run(["delete", catalogId]);
  }

  /** setListing always passes an explicit `--listed=<bool>` — `--listed` alone (no `=`) always means true. */
  async setListing(catalogId: string, listed: boolean): Promise<void> {
    await this.run(["set-listing", catalogId, `--listed=${listed}`]);
  }
}

/**
 * execInSpawn runs `cmd` non-interactively in a spawn's agent container via the NODE's `/exec`
 * endpoint (spawnctl exec, NOT proxied through the CP — needs `nodeAddr` directly reachable). A
 * non-zero exit code is returned as data (never thrown) so scenarios can assert on it.
 */
export async function execInSpawn(
  cfg: CliConfig,
  identity: Identity,
  nodeAddr: string,
  spawnId: string,
  cmd: string[],
): Promise<{ stdout: string; stderr: string; code: number }> {
  return execFileP2(
    cfg.spawnctlBin,
    buildExecArgs({ ...cfg, nodeAddr }, identity, spawnId, cmd),
    cfg.configHome,
  );
}

function writeTarField(buf: Buffer, s: string, offset: number, len: number): void {
  buf.write(s, offset, "ascii");
  void len; // caller guarantees s.length <= len; the field's remaining bytes stay zero-filled
}

function writeTarOctal(buf: Buffer, value: number, offset: number, len: number): void {
  // len-1 digits + a NUL terminator; the buffer is already zero-filled so the NUL is implicit.
  writeTarField(buf, value.toString(8).padStart(len - 1, "0"), offset, len - 1);
}

/**
 * buildSkillTar builds a minimal single-file POSIX ustar archive containing exactly one entry,
 * `SKILL.md`, whose body is `# acc skill\n<marker>\n`. This is the smallest payload
 * `validateSkillTar` (internal/cp/profiles_assembly.go) accepts for a custom-inline skill entry —
 * it only requires a readable TAR with a top-level `SKILL.md`. No external tar dependency.
 */
export function buildSkillTar(marker: string): Buffer {
  const body = Buffer.from(`# acc skill\n${marker}\n`, "utf8");

  const header = Buffer.alloc(512);
  writeTarField(header, "SKILL.md", 0, 100); // name
  writeTarOctal(header, 0o644, 100, 8); // mode
  writeTarOctal(header, 0, 108, 8); // uid
  writeTarOctal(header, 0, 116, 8); // gid
  writeTarOctal(header, body.length, 124, 12); // size
  writeTarOctal(header, Math.floor(Date.now() / 1000), 136, 12); // mtime
  header.fill(0x20, 148, 156); // chksum: 8 ASCII spaces while computing the sum
  header[156] = 0x30; // typeflag '0' = regular file
  writeTarField(header, "ustar", 257, 6); // magic ("ustar\0"; trailing NUL from the zero-fill)
  writeTarField(header, "00", 263, 2); // ustar version

  let sum = 0;
  for (let i = 0; i < header.length; i++) sum += header[i];
  writeTarField(header, sum.toString(8).padStart(6, "0") + "\0 ", 148, 8);

  const bodyPadded = Buffer.alloc(Math.ceil(body.length / 512) * 512);
  body.copy(bodyPadded);
  const trailer = Buffer.alloc(1024); // two zero blocks = end-of-archive marker

  return Buffer.concat([header, bodyPadded, trailer]);
}

/**
 * dummyAtRestEnvelope builds a base64-encoded JSON envelope carrying only the `at_rest` AAD
 * `internal/cp/secrets_catalog.go`'s `validateEnvelopeAAD` checks (`{account_id, secret_id,
 * version: 1}` — CreateSecret always validates against version 1). Ciphertext/recipient fields
 * are omitted: the CP does not decrypt or verify them at CRUD time, only at unseal (out of scope
 * for this black-box suite — see secrets.spec.ts).
 */
export function dummyAtRestEnvelope(owner: string, secretId: string): string {
  const json = JSON.stringify({ at_rest: { account_id: owner, secret_id: secretId, version: 1 } });
  return Buffer.from(json, "utf8").toString("base64");
}
