/**
 * Phase 6 — tenancy non-leakage: owner A never sees owner B's spawns, and vice versa, on both
 * the api (oracle) and cli surfaces. Uses its own dedicated ACC_TENANCY_A/B identities (falling
 * back to the shared ACC_IDENTITY_POOL's first two entries — see tenancy.ts / README.md),
 * bypassing the worker `identity` fixture entirely: this scenario is inherently two-owner, not
 * one-owner-per-worker.
 *
 * @mutating (creates spawns) — left UNTAGGED so the harness's default-deny guardrail treats it
 * as mutating and blocks it against a host that isn't on the non-prod allowlist.
 */

import { test, expect } from "../../src/harness/test";
import { AcceptanceClient } from "../../src/drivers/oracle";
import { CliDriver, parseListTable } from "../../src/drivers/cli";
import { loadTenancyConfig } from "../../src/scenarios/tenancy";
import type { DriverCtx } from "../../src/drivers/types";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

async function deviceSessionAccount(page: {
  request: { get(url: string): Promise<{ ok(): boolean; status(): number; text(): Promise<string> }> };
}, asOrigin: string): Promise<string> {
  const response = await page.request.get(`${asOrigin.replace(/\/$/, "")}/device/verify`);
  const html = await response.text();
  if (!response.ok()) throw new Error(`device session probe returned ${response.status()}: ${html}`);
  const account = /Logged in as <strong>([^<]+)<\/strong>/.exec(html)?.[1];
  if (!account) throw new Error("device session probe omitted the logged-in account");
  return account;
}

test("owner A sees only A's spawns; owner B sees only B's (api + cli)", async ({ target, auth, browser, ns }, testInfo) => {
  const cfg = loadTenancyConfig();
  const makeApi = async (identity: typeof cfg.a) => new AcceptanceClient({
    baseUrl: target.cpEndpoint,
    bearer: () => auth.cpAccessToken(identity),
    keyStore: await auth.sessionKeyStore(identity),
    getNodeAccessToken: () => auth.nodeAccessToken(identity),
    verifyTarget: auth.targetVerifier(identity),
  });
  const apiA = await makeApi(cfg.a);
  const apiB = await makeApi(cfg.b);
  const configA = await mkdtemp(join(tmpdir(), "spawnery-acc-tenancy-a-"));
  const configB = await mkdtemp(join(tmpdir(), "spawnery-acc-tenancy-b-"));
  const contextA = await browser.newContext({ baseURL: target.webOrigin });
  const contextB = await browser.newContext({ baseURL: target.webOrigin });
  const pageA = await contextA.newPage();
  const pageB = await contextB.newPage();
  await auth.seedWeb(pageA, cfg.a);
  await auth.seedWeb(pageB, cfg.b);
  let deviceSessionEvidence: { a: string; b: string } | undefined;
  if (target.authMode === "oauth-pop") {
    const [expectedA, expectedB, seededA, seededB] = await Promise.all([
      auth.accountId(cfg.a),
      auth.accountId(cfg.b),
      deviceSessionAccount(pageA, target.asOrigin),
      deviceSessionAccount(pageB, target.asOrigin),
    ]);
    expect(seededA).toBe(expectedA);
    expect(seededB).toBe(expectedB);
    expect(expectedA).not.toBe(expectedB);
    deviceSessionEvidence = { a: seededA, b: seededB };
  }
  const prepareCli = async (identity: typeof cfg.a, configHome: string, approvalPage: typeof pageA) => {
    const prepared = await auth.prepareCli(approvalPage, identity, {
      spawnctlBin: target.spawnctlBin,
      asOrigin: target.asOrigin,
      configHome,
    });
    return new CliDriver({
      cpEndpoint: target.cpEndpoint,
      spawnctlBin: target.spawnctlBin,
      authArgs: prepared.authArgs,
      configHome: prepared.configHome,
      trust: target.rootCAPath ? {
        rootCAPath: target.rootCAPath,
        trustDomain: target.trustDomain!,
        crlStatePath: target.crlStatePath!,
        crlIssuerPaths: target.crlIssuerPaths ?? [],
        crlPaths: target.crlPaths ?? [],
      } : undefined,
    });
  };
  const cliA = await prepareCli(cfg.a, configA, pageA);
  const cliB = await prepareCli(cfg.b, configB, pageB);
  const ctxA: DriverCtx = { identity: cfg.a, ns, api: apiA };
  const ctxB: DriverCtx = { identity: cfg.b, ns, api: apiB };

  const idA = await apiA.createSpawn({ appId: cfg.appId, model: cfg.model, name: ns("tenancy-a") });
  const idB = await apiB.createSpawn({ appId: cfg.appId, model: cfg.model, name: ns("tenancy-b") });

  try {
    const [listA, listB] = await Promise.all([apiA.listSpawns(), apiB.listSpawns()]);
    expect(listA).toContainSpawn(idA);
    expect(listA).not.toContainSpawn(idB);
    expect(listB).toContainSpawn(idB);
    expect(listB).not.toContainSpawn(idA);

    const [cliOutputA, cliOutputB] = await Promise.all([
      cliA.listOutput(ctxA),
      cliB.listOutput(ctxB),
    ]);
    const cliListA = parseListTable(cliOutputA.stdout);
    const cliListB = parseListTable(cliOutputB.stdout);
    await testInfo.attach("tenancy-cli-evidence.json", {
      contentType: "application/json",
      body: Buffer.from(JSON.stringify({
        deviceSessions: deviceSessionEvidence,
        cli: {
          a: { ...cliOutputA, parsed: cliListA },
          b: { ...cliOutputB, parsed: cliListB },
        },
      }, null, 2)),
    });

    expect(cliListA.some((s) => s.spawnId === idA)).toBe(true);
    expect(cliListA.some((s) => s.spawnId === idB)).toBe(false);
    expect(cliListB.some((s) => s.spawnId === idB)).toBe(true);
    expect(cliListB.some((s) => s.spawnId === idA)).toBe(false);
  } finally {
    await apiA.deleteSpawn(idA).catch(() => {});
    await apiB.deleteSpawn(idB).catch(() => {});
    await rm(configA, { recursive: true, force: true });
    await rm(configB, { recursive: true, force: true });
    await contextA.close();
    await contextB.close();
  }
});
