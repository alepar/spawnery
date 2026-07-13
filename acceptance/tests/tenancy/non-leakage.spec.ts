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
import { authv1 } from "@spawnery/client";
import { fromBinary } from "@bufbuild/protobuf";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

interface SessionIdentity {
  accountId: string;
  familyId: string;
  sessionKeyHash: string;
}

function decodeSessionIdentity(wire: string): SessionIdentity {
  const envelope = fromBinary(authv1.SignedAuthArtifactSchema, Buffer.from(wire, "base64url"));
  const body = fromBinary(authv1.SessionTokenBodySchema, envelope.payload);
  return {
    accountId: body.accountId,
    familyId: body.familyId,
    sessionKeyHash: Buffer.from(body.sessionKeyHash).toString("hex"),
  };
}

async function readStoredCliIdentity(configHome: string): Promise<SessionIdentity & { storedAccountId: string }> {
  const state = JSON.parse(await readFile(join(configHome, "spawnctl", "auth.json"), "utf8")) as {
    account_id?: string;
    cp_access_token?: string;
  };
  if (!state.cp_access_token) throw new Error(`missing cp_access_token in ${configHome}/spawnctl/auth.json`);
  return { ...decodeSessionIdentity(state.cp_access_token), storedAccountId: state.account_id ?? "" };
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

    const [apiIdentityA, apiIdentityB, cliIdentityA, cliIdentityB, cliOutputA, cliOutputB] = await Promise.all([
      auth.cpAccessToken(cfg.a).then(decodeSessionIdentity),
      auth.cpAccessToken(cfg.b).then(decodeSessionIdentity),
      readStoredCliIdentity(configA),
      readStoredCliIdentity(configB),
      cliA.listOutput(ctxA),
      cliB.listOutput(ctxB),
    ]);
    const cliListA = parseListTable(cliOutputA.stdout);
    const cliListB = parseListTable(cliOutputB.stdout);
    await testInfo.attach("tenancy-cli-evidence.json", {
      contentType: "application/json",
      body: Buffer.from(JSON.stringify({
        api: { a: apiIdentityA, b: apiIdentityB },
        cli: {
          a: { identity: cliIdentityA, ...cliOutputA, parsed: cliListA },
          b: { identity: cliIdentityB, ...cliOutputB, parsed: cliListB },
        },
      }, null, 2)),
    });

    expect(cliIdentityA.accountId).toBe(apiIdentityA.accountId);
    expect(cliIdentityB.accountId).toBe(apiIdentityB.accountId);
    expect(cliIdentityA.storedAccountId).toBe(cliIdentityA.accountId);
    expect(cliIdentityB.storedAccountId).toBe(cliIdentityB.accountId);
    expect(cliIdentityA.accountId).not.toBe(cliIdentityB.accountId);
    expect(cliIdentityA.familyId).not.toBe("");
    expect(cliIdentityB.familyId).not.toBe("");
    expect(cliIdentityA.familyId).not.toBe(cliIdentityB.familyId);
    expect(cliIdentityA.sessionKeyHash).not.toBe("");
    expect(cliIdentityB.sessionKeyHash).not.toBe("");
    expect(cliIdentityA.sessionKeyHash).not.toBe(cliIdentityB.sessionKeyHash);
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
