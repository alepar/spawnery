/**
 * marketDriver: the marketplace counterpart to drivers/{web,cli}.ts's SpawnDriver — browse/detail/
 * register/my-apps/listing, dual-surface (web + spawnctl). spawnctl's marketplace support is
 * register-only (cmd/spawnctl/main.go's `-register` root action); the other verbs are FAILING
 * STUBS on the cli surface, surfacing the CLI parity gap as visible red (mirrors cli.ts's
 * rename/suspend/stop/delete stubs — never a skip).
 *
 * CORRECTION vs the sp-tq0t.7 plan's grounding facts: app ids are NOT accepted verbatim.
 * internal/cp/validate.go's validateManifest requires exactly "creator/app" (two lowercase
 * [a-z0-9._-]+ segments) — a bare `ctx.ns("probe")` value (no slash) is rejected with
 * InvalidArgument. Namespaced app ids therefore use the `acc/<ns>` form (see market-fixtures.ts's
 * `marketAppId`), which satisfies the regex and keeps the run+worker-unique `ns()` segment
 * recognizable by `isAccArtifact` on its own.
 */

import { execFile as execFileCb } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type { Page } from "@playwright/test";
import { buildArgs, type CliConfig } from "./cli";
import type { DriverCtx } from "./types";

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

/** The default model recommended by every app the suite registers — mirrors examples/secret-app. */
const DEFAULT_MODEL = "anthropic/claude-3.5-sonnet";

export interface MountSpec {
  name: string;
  path: string;
  seed?: string;
}

export interface RegisterSpec {
  id: string;
  title: string;
  description?: string;
  tags?: string[];
  version: string;
  ref: string;
  mounts?: MountSpec[];
}

/** AppRow is the rendered projection of a catalog/my-apps card or row. */
export interface AppRow {
  id: string;
  tier?: string;
}

export interface MarketDriver {
  name: "web" | "cli";
  browse(ctx: DriverCtx, query?: string): Promise<AppRow[]>;
  openDetail(ctx: DriverCtx, appId: string): Promise<{ versions: string[] }>;
  register(ctx: DriverCtx, spec: RegisterSpec): Promise<{ appId: string; version: string }>;
  listMine(ctx: DriverCtx): Promise<AppRow[]>;
  setListing(ctx: DriverCtx, appId: string, listed: boolean): Promise<void>;
}

/** spawneryYaml builds a minimal spawneryapp.yml for `spec`, mirroring examples/secret-app's shape. */
export function spawneryYaml(spec: RegisterSpec): string {
  const mounts = spec.mounts ?? [{ name: "main", path: "data", seed: "seed" }];
  const lines: string[] = ["apiVersion: spawnery/v1", "kind: App", `id: ${spec.id}`, `title: ${JSON.stringify(spec.title)}`];
  if (spec.description) lines.push(`description: ${JSON.stringify(spec.description)}`);
  if (spec.tags && spec.tags.length > 0) {
    lines.push("tags:");
    for (const t of spec.tags) lines.push(`  - ${t}`);
  }
  lines.push("agents: { support: [any], requiresAcp: [prompt] }");
  lines.push(`model: { recommendedDefault: ${DEFAULT_MODEL} }`);
  if (mounts.length > 0) {
    lines.push("storage:");
    lines.push("  mounts:");
    for (const m of mounts) {
      lines.push(`    - name: ${m.name}`);
      lines.push(`      path: ${m.path}`);
      if (m.seed) lines.push(`      seed: ${m.seed}`);
    }
  }
  lines.push("visibility: open");
  return lines.join("\n") + "\n";
}

/** marketCliArgs builds the -register invocation's argv fragment (excluding the leading -cp/-token). */
export function marketCliArgs(spec: RegisterSpec, tmpDir: string): string[] {
  return ["-register", "-app", tmpDir, "-version", spec.version, "-ref", spec.ref];
}

/** parseRegisterOutput parses runRegister's `registered <appId>@<version> tier=<tier>` stdout line. */
export function parseRegisterOutput(stdout: string): { appId: string; version: string } | null {
  const m = /^registered\s+(\S+)@(\S+)\s+tier=/m.exec(stdout);
  if (!m) return null;
  return { appId: m[1], version: m[2] };
}

function parityGap(verb: string): Error {
  return new Error(`marketplace: spawnctl has no ${verb} (product parity gap sp-tq0t)`);
}

export class CliMarketDriver implements MarketDriver {
  readonly name = "cli" as const;

  constructor(private readonly cfg: CliConfig) {}

  async register(ctx: DriverCtx, spec: RegisterSpec): Promise<{ appId: string; version: string }> {
    const tmpDir = await mkdtemp(path.join(tmpdir(), "acc-market-"));
    try {
      await writeFile(path.join(tmpDir, "spawneryapp.yml"), spawneryYaml(spec));
      for (const m of spec.mounts ?? [{ name: "main", path: "data", seed: "seed" }]) {
        if (m.seed) await mkdir(path.join(tmpDir, m.seed), { recursive: true });
      }
      const { stdout } = await execFileP(this.cfg.spawnctlBin, buildArgs(this.cfg, ctx.identity, "", marketCliArgs(spec, tmpDir)));
      const parsed = parseRegisterOutput(stdout);
      if (!parsed) throw new Error(`marketDriver(cli): could not parse register output:\n${stdout}`);
      return parsed;
    } finally {
      await rm(tmpDir, { recursive: true, force: true });
    }
  }

  async browse(_ctx: DriverCtx, _query?: string): Promise<AppRow[]> {
    throw parityGap("browse");
  }

  async openDetail(_ctx: DriverCtx, _appId: string): Promise<{ versions: string[] }> {
    throw parityGap("openDetail");
  }

  async listMine(_ctx: DriverCtx): Promise<AppRow[]> {
    throw parityGap("listMine");
  }

  async setListing(_ctx: DriverCtx, _appId: string, _listed: boolean): Promise<void> {
    throw parityGap("setListing");
  }
}

function page(ctx: DriverCtx): Page {
  return ctx.page as Page;
}

/**
 * waitForConnectRpc waits for the response of the next `method` call to cp.v1.SpawnService.
 * MUST be created BEFORE the action that triggers the request (goto/click), then awaited after —
 * a pure DOM-text wait (e.g. "No apps found.") races the component's pre-fetch initial render,
 * which briefly renders the same empty state before its mount effect flips `loading` to true.
 */
function waitForConnectRpc(p: Page, method: string): Promise<unknown> {
  return p.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/cp.v1.SpawnService/${method}`));
}

export class WebMarketDriver implements MarketDriver {
  readonly name = "web" as const;

  async browse(ctx: DriverCtx, query?: string): Promise<AppRow[]> {
    const p = page(ctx);
    const loaded = waitForConnectRpc(p, "ListApps");
    await p.goto("/templates");
    await loaded;
    if (query) {
      const filtered = waitForConnectRpc(p, "ListApps");
      await p.getByTestId("market-search").fill(query);
      await p.getByTestId("market-search-btn").click();
      await filtered;
    }
    const cardsOrEmpty = p.locator('[data-testid^="app-card-"]').or(p.getByText("No apps found."));
    await cardsOrEmpty.first().waitFor({ state: "visible" });
    return readRows(p, "app-card-");
  }

  async openDetail(ctx: DriverCtx, appId: string): Promise<{ versions: string[] }> {
    const p = page(ctx);
    const loaded = waitForConnectRpc(p, "GetApp");
    await p.goto(`/templates/${encodeURIComponent(appId)}`);
    await loaded;
    await p.getByTestId("detail-back").waitFor({ state: "visible" });
    const versionsOrEmpty = p.locator('[data-testid^="version-"]').or(p.getByText("No versions."));
    await versionsOrEmpty.first().waitFor({ state: "visible" });
    const rows = p.locator('[data-testid^="version-"]');
    const count = await rows.count();
    const versions: string[] = [];
    for (let i = 0; i < count; i++) {
      const testid = await rows.nth(i).getAttribute("data-testid");
      versions.push(testid?.replace(/^version-/, "") ?? "");
    }
    return { versions };
  }

  async register(ctx: DriverCtx, spec: RegisterSpec): Promise<{ appId: string; version: string }> {
    const p = page(ctx);
    await p.goto("/publish");
    await p.getByTestId("publish-id").fill(spec.id);
    await p.getByTestId("publish-title").fill(spec.title);
    if (spec.description) await p.getByTestId("publish-description").fill(spec.description);
    if (spec.tags && spec.tags.length > 0) await p.getByTestId("publish-tags").fill(spec.tags.join(", "));
    await p.getByTestId("publish-version").fill(spec.version);
    await p.getByTestId("publish-ref").fill(spec.ref);

    const mounts = spec.mounts ?? [];
    for (let i = 0; i < mounts.length; i++) {
      if (i > 0) await p.getByTestId("publish-mount-add").click();
      await p.getByTestId(`publish-mount-name-${i}`).fill(mounts[i].name);
      await p.getByTestId(`publish-mount-path-${i}`).fill(mounts[i].path);
      if (mounts[i].seed) await p.getByTestId(`publish-mount-seed-${i}`).fill(mounts[i].seed as string);
    }

    await p.getByTestId("publish-submit").click();
    await p.waitForURL(/\/my-apps/);
    return { appId: spec.id, version: spec.version };
  }

  async listMine(ctx: DriverCtx): Promise<AppRow[]> {
    const p = page(ctx);
    const loaded = waitForConnectRpc(p, "ListMyApps");
    await p.goto("/my-apps");
    await loaded;
    const rowsOrEmpty = p.locator('[data-testid^="myapp-"]').or(p.getByText("You haven't published any apps yet."));
    await rowsOrEmpty.first().waitFor({ state: "visible" });
    return readRows(p, "myapp-");
  }

  async setListing(ctx: DriverCtx, appId: string, listed: boolean): Promise<void> {
    const p = page(ctx);
    await p.goto("/my-apps");
    const toggle = p.getByTestId(`listing-toggle-${appId}`);
    await toggle.waitFor({ state: "visible" });
    const current = (await toggle.getAttribute("aria-checked")) === "true";
    if (current === listed) return;
    await toggle.click();
    await p.waitForFunction(
      ({ appId, listed }) => {
        const el = document.querySelector(`[data-testid="listing-toggle-${appId}"]`);
        return el?.getAttribute("aria-checked") === String(listed);
      },
      { appId, listed },
    );
  }
}

/** readRows extracts {id,tier} rows from every element whose data-testid starts with `prefix`. */
async function readRows(p: Page, prefix: string): Promise<AppRow[]> {
  const rows = p.locator(`[data-testid^="${prefix}"]`);
  const count = await rows.count();
  const out: AppRow[] = [];
  for (let i = 0; i < count; i++) {
    const testid = await rows.nth(i).getAttribute("data-testid");
    const id = testid?.slice(prefix.length) ?? "";
    const tier = (await rows.nth(i).locator('[data-slot="badge"]').first().textContent().catch(() => null)) ?? undefined;
    out.push({ id, tier: tier?.trim() || undefined });
  }
  return out;
}
