import { Code, ConnectError } from "@connectrpc/connect";
import {
  buildIntentBodyBytes,
  buildSessionOpenSignedIntentB64,
  buildSignedIntent,
  cpv1,
  WebCryptoSessionSigner,
} from "@spawnery/client";
import { execFile } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { X509Certificate } from "node:crypto";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";
import { establishOAuthSession, type OAuthSessionState } from "../../src/auth/oauth-session";
import { createKnownVMTargetVerifier } from "../../src/drivers/oracle";
import { expect, test } from "../../src/harness/scenario";
import { waitForStatus } from "../../src/scenarios/wait";
import { acquirePendingAfterCapacityConverges } from "../../src/scenarios/pending-capacity";
import {
  cpClient,
  decodeSessionArtifact,
  loadVMAuthConfig,
  posixShellQuote,
  runtimeRootFingerprints,
  ssh,
  submitSpawn,
} from "./root-anchored-artifacts";

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
const execFileP = promisify(execFile);

async function pendingFor(client: ReturnType<typeof cpClient>, spawnId: string): Promise<cpv1.PendingIntent> {
  for (let attempt = 0; attempt < 120; attempt++) {
    const response = await client.getPendingIntent({ spawnId });
    if (response.ready && response.pending) return response.pending;
    await sleep(250);
  }
  throw new Error(`pending intent did not become ready for ${spawnId}`);
}

async function waitForNodeNACK(
  client: ReturnType<typeof cpClient>,
  spawnId: string,
  nack: string,
): Promise<void> {
  for (let attempt = 0; attempt < 120; attempt++) {
    const found = (await client.listSpawns({})).spawns.find((spawn) => spawn.spawnId === spawnId);
    if (found?.status === cpv1.SpawnStatus.ERROR) {
      expect(found.errorDetail).toMatch(new RegExp(`^failed_precondition: ${nack}:`));
      return;
    }
    await sleep(250);
  }
  throw new Error(`spawn ${spawnId} did not report ${nack}`);
}

async function expectNoRuntime(spawnId: string): Promise<void> {
  const cfg = loadVMAuthConfig();
  const pods = await ssh(cfg,
    `sudo crictl pods --label ${posixShellQuote(`spawnery.spawn-id=${spawnId}`)} -q`);
  expect(pods, `node created a runtime pod for rejected spawn ${spawnId}`).toBe("");
}

interface IntentMutation {
  domain?: string;
  op?: string;
  spawnId?: string;
  generation?: bigint;
  targetNodeId?: string;
  image?: string;
  model?: string;
  mounts?: Array<{
    name: string;
    backendUri: string;
    credentialSecretId?: string;
    createIfMissing?: boolean;
    repositoryId?: string;
  }>;
  attachedSecretIds?: string[];
  issuedAt?: number;
  corruptSignature?: boolean;
}

async function signedPendingIntent(
  pending: cpv1.PendingIntent,
  session: OAuthSessionState,
  mutation: IntentMutation = {},
) {
  const op = mutation.op ?? pending.op;
  const body = buildIntentBodyBytes({
    jti: crypto.randomUUID(),
    issuedAt: mutation.issuedAt ?? Math.floor(Date.now() / 1000),
    spawnId: mutation.spawnId ?? pending.spawnId,
    generation: mutation.generation ?? pending.generation,
    targetNodeId: mutation.targetNodeId ?? pending.targetNodeId,
    op,
    appRef: pending.appRef,
    image: mutation.image ?? pending.image,
    model: mutation.model ?? pending.model,
    dataRef: pending.dataRef,
    sessionId: "",
    mounts: mutation.mounts ?? pending.mounts,
    attachedSecretIds: mutation.attachedSecretIds ?? pending.attachedSecretIds,
  });
  const intent = await buildSignedIntent(
    pending.op,
    body,
    new WebCryptoSessionSigner(session.privateKey, session.publicKey),
  );
  if (mutation.domain) intent.domain = mutation.domain;
  if (mutation.corruptSignature) {
    const signature = intent.sig.slice();
    signature[0] ^= 0xff;
    intent.sig = signature;
  }
  return intent;
}

async function rejectedCreate(
  owner: OAuthSessionState,
  signingSession: OAuthSessionState,
  suffix: string,
  nack: string,
  mutation: IntentMutation = {},
  nodeToken = signingSession.nodeAccessToken,
): Promise<void> {
  const cfg = loadVMAuthConfig();
  const client = cpClient(cfg, owner.accessToken);
  const created = await client.createSpawn({ appId: cfg.appId, model: cfg.model, name: `acc-auth-${suffix}` });
  try {
    const pending = await pendingFor(client, created.spawnId);
    const intent = await signedPendingIntent(pending, signingSession, mutation);
    await client.submitIntent({ spawnId: created.spawnId, intent, nodeAccessToken: nodeToken });
    await waitForNodeNACK(client, created.spawnId, nack);
    await expectNoRuntime(created.spawnId);
  } finally {
    await client.deleteSpawn({ spawnId: created.spawnId }).catch(() => {});
  }
}

async function expectSession(
  client: ReturnType<typeof cpClient>,
  spawnId: string,
  transport: cpv1.SessionTransport,
): Promise<void> {
  await expect.poll(async () => (await client.listSessions({ spawnId })).sessions.some(
    (session) => session.transport === transport && session.status === "active",
  ), { timeout: 30_000 }).toBe(true);
}

async function activeSessionId(
  client: ReturnType<typeof cpClient>,
  spawnId: string,
  transport: cpv1.SessionTransport,
): Promise<string> {
  for (let attempt = 0; attempt < 120; attempt++) {
    const found = (await client.listSessions({ spawnId })).sessions.find(
      (session) => session.transport === transport && session.status === "active",
    );
    if (found) return found.sessionId || "0";
    await sleep(250);
  }
  throw new Error(`active session ${transport} did not appear for ${spawnId}`);
}

async function expectNodeJournalNACK(since: number, nack: string): Promise<void> {
  const cfg = loadVMAuthConfig();
  await expect.poll(async () => ssh(cfg,
    `sudo journalctl -u spawnery-node --since ${posixShellQuote(`@${since}`)} --no-pager | grep ${posixShellQuote(`nack=${nack}`)} || true`,
  ), { timeout: 30_000 }).toContain(`nack=${nack}`);
}

async function expectCliCRLFailure(
  spawnctlBin: string,
  configHome: string,
  args: string[],
  message: RegExp,
): Promise<void> {
  let error: unknown;
  try {
    await execFileP(spawnctlBin, args, {
      env: { ...process.env, XDG_CONFIG_HOME: configHome },
      maxBuffer: 1024 * 1024,
    });
  } catch (caught) {
    error = caught;
  }
  expect(error).toBeDefined();
  const output = `${(error as { stdout?: string }).stdout ?? ""}\n${(error as { stderr?: string }).stderr ?? ""}`;
  expect(output).toMatch(message);
}

test("production authorization: real web and stored-login CLI lifecycle", { tag: "@mutating" }, async ({
  page, web, cli, ctx, api, auth, identity, target, ns, spawns,
}) => {
  test.setTimeout(12 * 60_000);
  expect(target.authMode).toBe("oauth-pop");
  const cfg = loadVMAuthConfig();
  const deployed = await ssh(cfg, "sudo cat /etc/spawnery/env.d/common.env");
  expect(deployed).toContain("NODE_AUTH_MODE=enforced");
  expect(deployed).not.toMatch(/CP_DEV_AS_KEY|CP_AS_SESSION_PUBKEYS|NODE_AS_PUBKEYS/);
  expect(new Set(await runtimeRootFingerprints(cfg)).size).toBe(1);

  const [cpToken, nodeToken] = await Promise.all([
    auth.cpAccessToken(identity),
    auth.nodeAccessToken(identity),
  ]);
  const cpArtifact = decodeSessionArtifact(cpToken).body;
  const nodeArtifact = decodeSessionArtifact(nodeToken).body;
  expect(cpArtifact.audience).toBe("cp");
  expect(nodeArtifact.audience).toBe("node");
  expect(nodeArtifact.accountId).toBe(cpArtifact.accountId);
  expect(nodeArtifact.familyId).toBe(cpArtifact.familyId);
  expect(Buffer.from(nodeArtifact.sessionKeyHash)).toEqual(Buffer.from(cpArtifact.sessionKeyHash));
  const raw = cpClient(cfg, cpToken);

  const webId = spawns.track(await web.createSpawn(ctx, { appId: cfg.appId }));
  await web.waitActive(ctx, webId);
  await page.getByTestId("add-session").click();
  await page.getByTestId("new-session-goose-acp").click();
  await expectSession(raw, webId, cpv1.SessionTransport.ACP);
  expect(await api.findSpawn(webId)).toMatchObject({ spawnId: webId, status: "ACTIVE" });

  await page.getByTestId(`spawn-kebab-${webId}`).click();
  await page.getByTestId(`spawn-moveto-${webId}`).click();
  await expect(page.getByTestId("migrate-no-targets")).toBeVisible();
  await page.keyboard.press("Escape");

  await web.suspend(ctx, webId);
  await waitForStatus(api, webId, "SUSPENDED", { timeoutMs: 90_000 });
  await web.resume(ctx, webId);
  await web.waitActive(ctx, webId, { timeoutMs: 90_000 });
  const webFork = spawns.track(await web.fork(ctx, webId, { name: ns("prod-web-fork") }));
  await web.waitActive(ctx, webFork, { timeoutMs: 90_000 });
  expect(await api.findSpawn(webFork)).toMatchObject({ parentSpawnId: webId, status: "ACTIVE" });

  const cliId = spawns.track(await cli.createSpawn(ctx, { appId: cfg.appId, model: cfg.model }));
  await cli.waitActive(ctx, cliId, { timeoutMs: 90_000 });
  const execResult = await cli.exec(ctx, cliId, ["sh", "-lc", "printf production-session-open"]);
  expect(execResult).toMatchObject({ code: 0, stdout: "production-session-open" });

  await cli.suspend(ctx, cliId);
  await waitForStatus(api, cliId, "SUSPENDED", { timeoutMs: 90_000 });
  await cli.resume(ctx, cliId);
  await cli.waitActive(ctx, cliId, { timeoutMs: 90_000 });
  const beforeMove = await raw.getSpawnNodeKey({ spawnId: cliId });
  await cli.move(ctx, cliId, "node-1");
  await cli.waitActive(ctx, cliId, { timeoutMs: 90_000 });
  const afterMove = await raw.getSpawnNodeKey({ spawnId: cliId });
  expect(afterMove.targetNodeId).toBe("node-1");
  expect(afterMove.generation).toBeGreaterThan(beforeMove.generation);
  const cliFork = spawns.track(await cli.fork(ctx, cliId, { name: ns("prod-cli-fork") }));
  await cli.waitActive(ctx, cliFork, { timeoutMs: 90_000 });
  expect(await api.findSpawn(cliFork)).toMatchObject({ parentSpawnId: cliId, status: "ACTIVE" });
});

test("production authorization: exact node NACKs and target substitution refusal", { tag: "@mutating" }, async ({
  cli, cliConfigHome, ctx, identity, target,
}) => {
  test.setTimeout(12 * 60_000);
  const cfg = loadVMAuthConfig();
  if (target.identityPool.length < 2) throw new Error("production authorization requires two fake-GitHub accounts");
  const owner = await establishOAuthSession({
    asOrigin: cfg.asOrigin,
    redirectUri: `${cfg.webOrigin}/callback`,
    loginHint: identity.token,
  });
  const other = await establishOAuthSession({
    asOrigin: cfg.asOrigin,
    redirectUri: `${cfg.webOrigin}/callback`,
    loginHint: target.identityPool[1].token,
  });

  const client = cpClient(cfg, owner.accessToken);
  await cli.list(ctx);

  const crlProbe = await mkdtemp(join(tmpdir(), "spawnery-acc-crl-probe-"));
  try {
    const commonArgs = [
      "move", "does-not-exist", "node-1",
      "--cp", target.cpEndpoint,
      "--config-dir", join(cliConfigHome, "spawnctl"),
      "--root-ca", target.rootCAPath!,
      "--trust-domain", target.trustDomain!,
      "--crl-issuer", target.crlIssuerPaths![0],
    ];
    await expectCliCRLFailure(target.spawnctlBin, cliConfigHome, [
      ...commonArgs, "--crl-state", join(crlProbe, "missing", "state.json"),
    ], /certificate revocation state has no current CRL/);

    const expiredCRL = await ssh(cfg, `set -eu; d=$(mktemp -d); trap 'rm -rf "$d"' EXIT; touch "$d/index"; printf '02\\n' > "$d/crlnumber"; printf '%s\\n' '[ca]' 'default_ca=issuer' '[issuer]' 'database='"$d"'/index' 'new_certs_dir='"$d" 'certificate=/etc/spawnery/node/service-intermediate.pem' 'private_key=/var/lib/spawnery-offline/service-intermediate-key.pem' 'default_md=sha256' 'default_crl_days=30' 'crlnumber='"$d"'/crlnumber' 'crl_extensions=crl_ext' '[crl_ext]' 'authorityKeyIdentifier=keyid:always' > "$d/openssl.cnf"; last=$(date -u -d '15 seconds ago' +%Y%m%d%H%M%SZ); next=$(date -u -d '5 seconds ago' +%Y%m%d%H%M%SZ); sudo openssl ca -gencrl -batch -config "$d/openssl.cnf" -crl_lastupdate "$last" -crl_nextupdate "$next" -out "$d/expired.pem" >/dev/null 2>&1; cat "$d/expired.pem"`);
    const expiredPath = join(crlProbe, "expired.pem");
    await writeFile(expiredPath, `${expiredCRL}\n`, { mode: 0o600 });
    await expectCliCRLFailure(target.spawnctlBin, cliConfigHome, [
      ...commonArgs,
      "--crl-state", join(crlProbe, "expired", "state.json"),
      "--crl", expiredPath,
    ], /CRL is expired/);
  } finally {
    await rm(crlProbe, { recursive: true, force: true });
  }

  for (const missing of ["node_access_token", "intent"] as const) {
    // The prior lifecycle test confirms its CP deletions before this test begins, but node slot
    // retirement converges asynchronously. Retry only that exact bounded capacity terminal.
    const { created, pending } = await acquirePendingAfterCapacityConverges(client, {
      appId: cfg.appId, model: cfg.model, name: `acc-auth-missing-${missing}`,
    });
    try {
      const intent = await signedPendingIntent(pending, owner);
      let error: unknown;
      try {
        await client.submitIntent({
          spawnId: created.spawnId,
          intent: missing === "intent" ? undefined : intent,
          nodeAccessToken: missing === "node_access_token" ? "" : owner.nodeAccessToken,
        });
      } catch (caught) {
        error = caught;
      }
      expect(error).toBeInstanceOf(ConnectError);
      expect((error as ConnectError).code).toBe(Code.InvalidArgument);
      expect((error as ConnectError).rawMessage).toBe(`${missing} required`);
      await expectNoRuntime(created.spawnId);

      // Complete the pending create deterministically so deletion does not wait for the 2m5s
      // SignedIntent TTL. This cleanup NACK is separate from the missing-field assertion above.
      const cleanupIntent = await signedPendingIntent(pending, owner, { corruptSignature: true });
      await client.submitIntent({
        spawnId: created.spawnId,
        intent: cleanupIntent,
        nodeAccessToken: owner.nodeAccessToken,
      });
      await waitForNodeNACK(client, created.spawnId, "BAD_SIG");
      await expectNoRuntime(created.spawnId);
    } finally {
      await client.deleteSpawn({ spawnId: created.spawnId }).catch(() => {});
    }
  }

  const wrongAudienceSince = Math.floor(Date.now() / 1000);
  await rejectedCreate(owner, owner, "wrong-audience", "WRONG_AUDIENCE", {}, owner.accessToken);
  await expectNodeJournalNACK(wrongAudienceSince, "WRONG_AUDIENCE");
  await rejectedCreate(owner, other, "cnf-mismatch", "CNF_MISMATCH", {}, owner.nodeAccessToken);
  await rejectedCreate(owner, owner, "bad-signature", "BAD_SIG", { corruptSignature: true });
  await rejectedCreate(owner, other, "owner-mismatch-web", "OWNER_MISMATCH");
  await rejectedCreate(owner, other, "owner-mismatch-cli", "OWNER_MISMATCH");

  const correspondence: Array<[string, IntentMutation]> = [
    ["domain", { domain: "spawnery/intent/resume-spawn/v1" }],
    ["operation", { op: "resume-spawn" }],
    ["spawn", { spawnId: "substituted-spawn" }],
    ["target", { targetNodeId: "node-elsewhere" }],
    ["generation", { generation: 999_999n }],
    ["image", { image: "registry.invalid/substituted@sha256:deadbeef" }],
    ["model", { model: "substituted/model" }],
    ["mount", { mounts: [{ name: "substituted", backendUri: "gh:elsewhere/repo" }] }],
    ["secret", { attachedSecretIds: ["substituted-secret"] }],
  ];
  for (const [name, mutation] of correspondence) {
    await rejectedCreate(owner, owner, `correspondence-${name}`, "CORRESPONDENCE", mutation);
  }
  const now = Math.floor(Date.now() / 1000);
  await rejectedCreate(owner, owner, "stale", "STALE", { issuedAt: now - 121 });
  await rejectedCreate(owner, owner, "skew", "SKEW", { issuedAt: now + 31 });

  const accepted = await client.createSpawn({ appId: cfg.appId, model: cfg.model, name: "acc-auth-target-pins" });
  try {
    const pending = await pendingFor(client, accepted.spawnId);
    const response = await client.getPendingIntent({ spawnId: accepted.spawnId });
    const realTarget = {
      nodeCertChain: response.nodeCertChain,
      targetNodeId: response.targetNodeId,
      targetNodeClass: response.targetNodeClass,
      targetNodeAccountId: response.targetNodeAccountId,
    };
    const rootCAPEM = await readFile(target.rootCAPath!, "utf8");
    const verifier = createKnownVMTargetVerifier({
      rootCAPEM,
      trustDomain: target.trustDomain!,
      expectedNodeId: "node-1",
      expectedNodeClass: "cloud",
      expectedNodeAccountId: target.cloudAccountId!,
    });
    await expect(verifier(realTarget)).resolves.toBeUndefined();
    for (const altered of [
      { ...realTarget, targetNodeId: "node-elsewhere" },
      { ...realTarget, targetNodeClass: "self-hosted" },
      { ...realTarget, targetNodeAccountId: "other-system-account" },
      { ...realTarget, nodeCertChain: new Uint8Array() },
    ]) {
      await expect(verifier(altered)).rejects.toThrow();
    }
    const leafPEM = new TextDecoder().decode(realTarget.nodeCertChain)
      .match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/)?.[0];
    if (!leafPEM) throw new Error("real target did not contain a leaf certificate");
    expect(() => new X509Certificate(leafPEM)).not.toThrow();
    const untrusted = createKnownVMTargetVerifier({
      rootCAPEM: leafPEM,
      trustDomain: target.trustDomain!,
      expectedNodeId: "node-1",
      expectedNodeClass: "cloud",
      expectedNodeAccountId: target.cloudAccountId!,
    });
    await expect(untrusted(realTarget)).rejects.toThrow(/not rooted/);
    await expectNoRuntime(accepted.spawnId);
    expect(pending.spawnId).toBe(accepted.spawnId);
  } finally {
    await client.deleteSpawn({ spawnId: accepted.spawnId }).catch(() => {});
  }
});

test("production authorization: session-open replay leaves the original attachment live", { tag: "@mutating" }, async ({ page }) => {
  test.setTimeout(4 * 60_000);
  const cfg = loadVMAuthConfig();
  const session = await establishOAuthSession({
    asOrigin: cfg.asOrigin,
    redirectUri: `${cfg.webOrigin}/callback`,
    loginHint: cfg.owner,
  });
  const accepted = await submitSpawn(
    cfg,
    session.nodeAccessToken,
    { privateKey: session.privateKey, publicKey: session.publicKey },
    "session-replay",
    session.accessToken,
  );
  expect(accepted.status).toBe("ACTIVE");
  const client = cpClient(cfg, session.accessToken);
  try {
    await client.createSession({
      spawnId: accepted.spawnId,
      transport: cpv1.SessionTransport.MOSH,
      runnable: "shell",
    });
    const sessionId = await activeSessionId(client, accepted.spawnId, cpv1.SessionTransport.MOSH);
    const target = await client.getSpawnNodeKey({ spawnId: accepted.spawnId });
    const signedIntent = await buildSessionOpenSignedIntentB64(
      accepted.spawnId,
      sessionId,
      target.generation,
      target.targetNodeId,
      new WebCryptoSessionSigner(session.privateKey, session.publicKey),
    );
    const since = Math.floor(Date.now() / 1000) - 1;
    const wsUrl = `${cfg.webOrigin.replace(/^http/, "ws")}/ws/session`;
    await page.evaluate(async ({ url, common, intent }) => {
      const holder = window as typeof window & { __productionAuthSockets?: Record<string, WebSocket> };
      holder.__productionAuthSockets = {};
      const bind = async (name: string, clientId: string, signed: string) => {
        const socket = new WebSocket(url);
        holder.__productionAuthSockets![name] = socket;
        await new Promise<void>((resolve, reject) => {
          const timeout = window.setTimeout(() => reject(new Error(`${name} did not open`)), 10_000);
          socket.addEventListener("open", () => {
            window.clearTimeout(timeout);
            socket.send(JSON.stringify({ ...common, clientId, signedIntent: signed }));
            resolve();
          }, { once: true });
          socket.addEventListener("error", () => reject(new Error(`${name} failed`)), { once: true });
        });
      };
      await bind("original", `acc-original-${crypto.randomUUID()}`, intent);
      await new Promise((resolve) => window.setTimeout(resolve, 1_000));
      await bind("replay", `acc-replay-${crypto.randomUUID()}`, intent);
    }, {
      url: wsUrl,
      intent: signedIntent,
      common: {
        spawnId: accepted.spawnId,
        token: session.accessToken,
        nodeAccessToken: session.nodeAccessToken,
        sessionId,
        cursor: 0,
      },
    });
    await expectNodeJournalNACK(since, "REPLAY");
    await expect.poll(async () => page.evaluate(() => {
      const holder = window as typeof window & { __productionAuthSockets?: Record<string, WebSocket> };
      return holder.__productionAuthSockets?.original?.readyState;
    })).toBe(1);
    expect((await client.listSessions({ spawnId: accepted.spawnId })).sessions.filter(
      (item) => item.sessionId === sessionId && item.status === "active",
    )).toHaveLength(1);
  } finally {
    await page.evaluate(() => {
      const holder = window as typeof window & { __productionAuthSockets?: Record<string, WebSocket> };
      Object.values(holder.__productionAuthSockets ?? {}).forEach((socket) => socket.close());
    }).catch(() => {});
    await client.deleteSpawn({ spawnId: accepted.spawnId }).catch(() => {});
  }
});
