import { describe, it, expect, vi, beforeEach } from "vitest";
import { ProfileCli, CatalogCli, execInSpawn, buildSkillTar, dummyAtRestEnvelope } from "./customization";
import type { Identity } from "../fixtures/identity-pool";

vi.mock("node:child_process", () => ({
  execFile: vi.fn(),
}));

const identity: Identity = { token: "tok-123", owner: "acc-owner-1" };
const cfg = { cpEndpoint: "https://cp.example", spawnctlBin: "spawnctl" };

async function mockExecFile(impl: (args: string[]) => { stdout?: string; stderr?: string; err?: Error & { code?: number | string } }): Promise<void> {
  const cp = await import("node:child_process");
  vi.mocked(cp.execFile).mockImplementation(((_bin: string, args: string[], cb: (...a: unknown[]) => void) => {
    const { stdout = "", stderr = "", err } = impl(args);
    cb(err ?? null, stdout, stderr);
  }) as unknown as typeof cp.execFile);
}

beforeEach(async () => {
  const cp = await import("node:child_process");
  vi.mocked(cp.execFile).mockReset();
});

describe("ProfileCli", () => {
  it("create builds `profile create <name> -cp -token` and parses the profile id", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "created profile prof-abc (version 1)\n" };
    });

    const cli = new ProfileCli(cfg, identity);
    const id = await cli.create("my-profile");

    expect(seenArgs).toEqual(["profile", "create", "my-profile", "-cp", "https://cp.example", "-token", "tok-123"]);
    expect(id).toBe("prof-abc");
  });

  it("create throws when the profile id can't be parsed", async () => {
    await mockExecFile(() => ({ stdout: "unexpected output\n" }));
    const cli = new ProfileCli(cfg, identity);
    await expect(cli.create("my-profile")).rejects.toThrow(/could not parse profile id/);
  });

  it("entryAddCustom emits entry add <id> --kind --name --custom-file (+ -cp/-token trailing)", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "added entry entry-1 (profile version 2)\n" };
    });

    const cli = new ProfileCli(cfg, identity);
    const entryId = await cli.entryAddCustom("prof-abc", { kind: "skill", name: "acc-skill", customFilePath: "/tmp/skill.tar" });

    expect(seenArgs).toEqual([
      "profile",
      "entry",
      "add",
      "prof-abc",
      "--kind",
      "skill",
      "--name",
      "acc-skill",
      "--custom-file",
      "/tmp/skill.tar",
      "-cp",
      "https://cp.example",
      "-token",
      "tok-123",
    ]);
    expect(entryId).toBe("entry-1");
  });

  it("entryAddCatalog emits entry add <id> --kind --name --catalog", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "added entry entry-2 (profile version 3)\n" };
    });

    const cli = new ProfileCli(cfg, identity);
    await cli.entryAddCatalog("prof-abc", { kind: "skill", name: "acc-skill", catalogId: "cat-1" });

    expect(seenArgs).toEqual([
      "profile",
      "entry",
      "add",
      "prof-abc",
      "--kind",
      "skill",
      "--name",
      "acc-skill",
      "--catalog",
      "cat-1",
      "-cp",
      "https://cp.example",
      "-token",
      "tok-123",
    ]);
  });

  it("secretAdd/secretRemove build profile secret add|remove <id> <secretId>", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "added secret ref sec-1 (profile version 4)\n" };
    });

    const cli = new ProfileCli(cfg, identity);
    await cli.secretAdd("prof-abc", "sec-1");
    expect(seenArgs).toEqual(["profile", "secret", "add", "prof-abc", "sec-1", "-cp", "https://cp.example", "-token", "tok-123"]);

    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "removed secret ref sec-1 (profile version 5)\n" };
    });
    await cli.secretRemove("prof-abc", "sec-1");
    expect(seenArgs).toEqual(["profile", "secret", "remove", "prof-abc", "sec-1", "-cp", "https://cp.example", "-token", "tok-123"]);
  });

  it("run() throws on a non-zero exit, including stdout/stderr in the message", async () => {
    await mockExecFile(() => ({
      stdout: "some output\n",
      stderr: "boom\n",
      err: Object.assign(new Error("exit 1"), { code: 1 }),
    }));
    const cli = new ProfileCli(cfg, identity);
    await expect(cli.delete("prof-abc")).rejects.toThrow(/exited 1[\s\S]*boom/);
  });
});

describe("CatalogCli", () => {
  it("create builds `catalog create <name> --kind <k> ...` and parses the catalog id", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "created catalog entry cat-xyz\n" };
    });

    const cli = new CatalogCli(cfg, identity);
    const id = await cli.create({ name: "my-cat", kind: "skill", description: "desc", contentFilePath: "/tmp/c.tar" });

    expect(seenArgs).toEqual([
      "catalog",
      "create",
      "my-cat",
      "--kind",
      "skill",
      "--description",
      "desc",
      "--content-file",
      "/tmp/c.tar",
      "-cp",
      "https://cp.example",
      "-token",
      "tok-123",
    ]);
    expect(id).toBe("cat-xyz");
  });

  it("create omits optional flags when not given", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "created catalog entry cat-2\n" };
    });
    const cli = new CatalogCli(cfg, identity);
    await cli.create({ name: "my-cat", kind: "skill" });
    expect(seenArgs).toEqual(["catalog", "create", "my-cat", "--kind", "skill", "-cp", "https://cp.example", "-token", "tok-123"]);
  });

  it("create throws when the catalog id can't be parsed", async () => {
    await mockExecFile(() => ({ stdout: "unexpected\n" }));
    const cli = new CatalogCli(cfg, identity);
    await expect(cli.create({ name: "my-cat", kind: "skill" })).rejects.toThrow(/could not parse catalog id/);
  });

  it("setListing always passes an explicit --listed=<bool>", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "set listing=true for catalog entry cat-xyz\n" };
    });
    const cli = new CatalogCli(cfg, identity);
    await cli.setListing("cat-xyz", true);
    expect(seenArgs).toEqual(["catalog", "set-listing", "cat-xyz", "--listed=true", "-cp", "https://cp.example", "-token", "tok-123"]);

    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "set listing=false for catalog entry cat-xyz\n" };
    });
    await cli.setListing("cat-xyz", false);
    expect(seenArgs).toEqual(["catalog", "set-listing", "cat-xyz", "--listed=false", "-cp", "https://cp.example", "-token", "tok-123"]);
  });
});

describe("execInSpawn", () => {
  it("uses the shared exec argument builder and returns stdout/stderr/code", async () => {
    let seenArgs: string[] = [];
    await mockExecFile((args) => {
      seenArgs = args;
      return { stdout: "hello\n", stderr: "" };
    });

		const result = await execInSpawn(cfg, identity, "spawn-1", ["sh", "-lc", "echo hi"]);

    expect(seenArgs).toEqual([
      "exec",
      "-spawn",
      "spawn-1",
			"-cp",
      "https://cp.example",
      "-token",
      "tok-123",
      "--",
      "sh",
      "-lc",
      "echo hi",
    ]);
    expect(result).toEqual({ stdout: "hello\n", stderr: "", code: 0 });
  });

  it("returns a non-zero exit code as data, not a throw", async () => {
    await mockExecFile(() => ({
      stdout: "partial\n",
      stderr: "failed\n",
      err: Object.assign(new Error("Command failed"), { code: 7 }),
    }));

		const result = await execInSpawn(cfg, identity, "spawn-1", ["false"]);
    expect(result).toEqual({ stdout: "partial\n", stderr: "failed\n", code: 7 });
  });

  it("falls back to code 1 when the underlying error has no numeric code (e.g. spawn failure)", async () => {
    await mockExecFile(() => ({
      stdout: "",
      stderr: "",
      err: Object.assign(new Error("spawn ENOENT"), { code: "ENOENT" }),
    }));

		const result = await execInSpawn(cfg, identity, "spawn-1", ["true"]);
    expect(result.code).toBe(1);
  });
});

describe("buildSkillTar", () => {
  it("produces a 512-byte-multiple archive containing SKILL.md and the marker", () => {
    const tar = buildSkillTar("MARKER=run-42");
    expect(tar.length % 512).toBe(0);
    expect(tar.subarray(0, 100).toString("ascii").replace(/\0+$/, "")).toBe("SKILL.md");
    expect(tar.toString("utf8")).toContain("MARKER=run-42");
  });

  it("has a valid ustar checksum in its first header block", () => {
    const tar = buildSkillTar("marker-x");
    const header = Buffer.from(tar.subarray(0, 512));

    // Recompute independently: sum all 512 header bytes with the chksum field (148..156)
    // replaced by ASCII spaces, per the POSIX ustar spec.
    const recomputed = Buffer.from(header);
    recomputed.fill(0x20, 148, 156);
    let sum = 0;
    for (let i = 0; i < recomputed.length; i++) sum += recomputed[i];

    const encoded = header.subarray(148, 156).toString("ascii");
    const encodedValue = parseInt(encoded.replace(/[\0 ]+$/, ""), 8);
    expect(encodedValue).toBe(sum);

    // magic field
    expect(header.subarray(257, 263).toString("ascii")).toBe("ustar\0");
  });

  it("produces a fresh marker per call (no accidental caching)", () => {
    const a = buildSkillTar("marker-a");
    const b = buildSkillTar("marker-b");
    expect(a.toString("utf8")).toContain("marker-a");
    expect(b.toString("utf8")).toContain("marker-b");
    expect(a.toString("utf8")).not.toContain("marker-b");
  });
});

describe("dummyAtRestEnvelope", () => {
  it("base64-decodes to exactly {at_rest: {account_id, secret_id, version: 1}}", () => {
    const encoded = dummyAtRestEnvelope("acc-owner-1", "sec-1");
    const decoded = JSON.parse(Buffer.from(encoded, "base64").toString("utf8"));
    expect(decoded).toEqual({ at_rest: { account_id: "acc-owner-1", secret_id: "sec-1", version: 1 } });
  });
});
