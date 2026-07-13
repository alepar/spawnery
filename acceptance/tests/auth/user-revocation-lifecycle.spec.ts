import { expect, test, type Page } from "@playwright/test";
import {
  buildSessionReauthSignedIntentB64,
  buildSessionOpenSignedIntentB64,
  cpv1,
  WebCryptoSessionSigner,
} from "@spawnery/client";
import { refreshOAuthSession } from "../../src/auth/oauth-session";
import { restoreAuthService } from "./auth-service-restoration";
import {
  aggregateLifecycleFailures,
  cleanupSpawnFailures,
  lifecycleFailure,
  NO_LIFECYCLE_FAILURE,
  type LifecycleFailureState,
} from "./user-revocation-lifecycle";
import {
  cpClient,
  decodeSessionArtifact,
  establishCurrentSession,
  loadVMAuthConfig,
  ssh,
  submitSpawn,
  type VMAuthConfig,
} from "./root-anchored-artifacts";

const SOCKETS_KEY = "__spawneryRevocationSockets";

interface SessionBinding {
  clientId: string;
}

interface AuthorizationCloseRecord {
  timestampMs: number;
  spawnId: string;
  generation: number;
  sessionId: string;
  clientId: string;
  attachmentId: string;
  reason: string;
}

async function waitForSession(
  cfg: VMAuthConfig,
  token: string,
  spawnId: string,
  transport: cpv1.SessionTransport,
): Promise<string> {
  const client = cpClient(cfg, token);
  for (let attempt = 0; attempt < 120; attempt++) {
    const found = (await client.listSessions({ spawnId })).sessions.find(
      (session) => session.transport === transport && session.status === "active",
    );
    if (found) return found.sessionId || "0";
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`session transport ${transport} did not become active for ${spawnId}`);
}

async function bindSession(
  page: Page,
  cfg: VMAuthConfig,
  session: Awaited<ReturnType<typeof establishCurrentSession>>,
  nodeAccessToken: string,
  spawnId: string,
  sessionId: string,
  generation: bigint,
  targetNodeId: string,
  name: string,
): Promise<SessionBinding> {
  const intent = await buildSessionOpenSignedIntentB64(
    spawnId,
    sessionId,
    generation,
    targetNodeId,
    new WebCryptoSessionSigner(session.privateKey, session.publicKey),
  );
  const clientId = `acc-revocation-${name}-${crypto.randomUUID()}`;
  const wsOrigin = cfg.webOrigin.replace(/^http/, "ws");
  await page.evaluate(async ({ wsUrl, bind, socketName, socketsKey }) => {
    const holder = window as typeof window & Record<string, Record<string, WebSocket>>;
    holder[socketsKey] ??= {};
    const socket = new WebSocket(wsUrl);
    holder[socketsKey][socketName] = socket;
    await new Promise<void>((resolve, reject) => {
      const timeout = window.setTimeout(() => reject(new Error(`socket ${socketName} did not open`)), 10_000);
      socket.addEventListener("open", () => {
        socket.send(JSON.stringify(bind));
        window.setTimeout(() => {
          window.clearTimeout(timeout);
          if (socket.readyState === WebSocket.OPEN) resolve();
          else reject(new Error(`socket ${socketName} closed during node bind`));
        }, 2_000);
      }, { once: true });
      socket.addEventListener("error", () => {
        window.clearTimeout(timeout);
        reject(new Error(`socket ${socketName} failed`));
      }, { once: true });
    });
  }, {
    wsUrl: `${wsOrigin}/ws/session`,
    socketName: name,
    socketsKey: SOCKETS_KEY,
    bind: {
      spawnId,
      token: session.accessToken,
      nodeAccessToken,
      clientId,
      sessionId,
      cursor: 0,
      signedIntent: intent,
    },
  });
  return { clientId };
}

function textAttribute(message: string, name: string): string | undefined {
  const match = message.match(new RegExp(`(?:^|\\s)${name}=("(?:[^"\\\\]|\\\\.)*"|\\S+)`));
  if (!match) return undefined;
  if (!match[1].startsWith('"')) return match[1];
  try {
    return JSON.parse(match[1]) as string;
  } catch {
    return undefined;
  }
}

function parseAuthorizationCloseJournal(output: string): AuthorizationCloseRecord[] {
  const records: AuthorizationCloseRecord[] = [];
  for (const line of output.split("\n")) {
    if (!line.trim()) continue;
    const journal = JSON.parse(line) as { MESSAGE?: string; __REALTIME_TIMESTAMP?: string };
    const message = String(journal.MESSAGE ?? "");
    let fields: Record<string, unknown>;
    try {
      fields = JSON.parse(message) as Record<string, unknown>;
    } catch {
      fields = {
        msg: textAttribute(message, "msg"),
        spawn_id: textAttribute(message, "spawn_id"),
        generation: textAttribute(message, "generation"),
        session_id: textAttribute(message, "session_id"),
        client_id: textAttribute(message, "client_id"),
        attachment_id: textAttribute(message, "attachment_id"),
        reason: textAttribute(message, "reason"),
      };
    }
    if (fields.msg !== "session_authorization_closed") continue;
    records.push({
      timestampMs: Number(journal.__REALTIME_TIMESTAMP ?? 0) / 1000,
      spawnId: String(fields.spawn_id ?? ""),
      generation: Number(fields.generation ?? 0),
      sessionId: String(fields.session_id ?? ""),
      clientId: String(fields.client_id ?? ""),
      attachmentId: String(fields.attachment_id ?? ""),
      reason: String(fields.reason ?? ""),
    });
  }
  return records;
}

async function authorizationCloseRecords(
  cfg: VMAuthConfig,
  sinceEpoch: number,
): Promise<AuthorizationCloseRecord[]> {
  const output = await ssh(
    cfg,
    `sudo journalctl -u spawnery-node --since @${sinceEpoch} --output=json --no-pager`,
  );
  return parseAuthorizationCloseJournal(output);
}

async function expectAuthorizationClosed(
  cfg: VMAuthConfig,
  sinceEpoch: number,
  expected: Omit<AuthorizationCloseRecord, "timestampMs" | "attachmentId">,
  timeoutMs: number,
): Promise<AuthorizationCloseRecord> {
  const matches = async () => (await authorizationCloseRecords(cfg, sinceEpoch)).filter((record) =>
    record.spawnId === expected.spawnId && record.generation === expected.generation &&
    record.sessionId === expected.sessionId && record.clientId === expected.clientId &&
    record.reason === expected.reason);
  await expect.poll(async () => (await matches()).length, { timeout: timeoutMs }).toBe(1);
  const [record] = await matches();
  expect(record.attachmentId).not.toBe("");
  return record;
}

async function expectSocketsClosed(page: Page, names: string[], timeoutMs: number): Promise<void> {
  await expect.poll(async () => page.evaluate(({ socketsKey, socketNames }) => {
    const holder = window as typeof window & Record<string, Record<string, WebSocket>>;
    return socketNames.map((name) => holder[socketsKey]?.[name]?.readyState);
  }, { socketsKey: SOCKETS_KEY, socketNames: names }), { timeout: timeoutMs }).toEqual(
    names.map(() => 3),
  );
}

async function reauthenticateSocket(
  page: Page,
  session: Awaited<ReturnType<typeof establishCurrentSession>>,
  spawnId: string,
  sessionId: string,
  generation: bigint,
  targetNodeId: string,
  socketName: string,
): Promise<void> {
  const tokenId = decodeSessionArtifact(session.nodeAccessToken).body.tokenId;
  const signedIntent = await buildSessionReauthSignedIntentB64({
    spawnId,
    sessionId,
    generation,
    targetNodeId,
    newTokenId: tokenId,
  }, new WebCryptoSessionSigner(session.privateKey, session.publicKey));
  await page.evaluate(({ socketsKey, name, control }) => {
    const holder = window as typeof window & Record<string, Record<string, WebSocket>>;
    const socket = holder[socketsKey]?.[name];
    if (!socket || socket.readyState !== WebSocket.OPEN) throw new Error(`socket ${name} is not open for reauth`);
    socket.send(JSON.stringify(control));
  }, {
    socketsKey: SOCKETS_KEY,
    name: socketName,
    control: { type: "nodeReauth", nodeAccessToken: session.nodeAccessToken, signedIntent },
  });
}

async function expectSocketsOpen(page: Page, names: string[]): Promise<void> {
  await expect.poll(async () => page.evaluate(({ socketsKey, socketNames }) => {
    const holder = window as typeof window & Record<string, Record<string, WebSocket>>;
    return socketNames.map((name) => holder[socketsKey]?.[name]?.readyState);
  }, { socketsKey: SOCKETS_KEY, socketNames: names })).toEqual(names.map(() => 1));
}

test("user-revocation-lifecycle: logout closes ACP and MOSH; AS outage cannot extend node expiry", async ({ page }) => {
  test.setTimeout(22 * 60_000);
  const cfg = loadVMAuthConfig();
  await page.goto(cfg.webOrigin);
  const createdSpawnIds: string[] = [];
  let authsvcStopped = false;
  let cleanupToken = "";
  let testError: LifecycleFailureState = NO_LIFECYCLE_FAILURE;
  let restorationError: LifecycleFailureState = NO_LIFECYCLE_FAILURE;
  let cleanupErrors: unknown[] = [];

  try {
    let logoutSession = await establishCurrentSession(cfg);
    cleanupToken = logoutSession.accessToken;
    const logoutKeyPair = { privateKey: logoutSession.privateKey, publicKey: logoutSession.publicKey };
    const live = await submitSpawn(
      cfg,
      logoutSession.nodeAccessToken,
      logoutKeyPair,
      "logout-lifecycle",
      logoutSession.accessToken,
    );
    createdSpawnIds.push(live.spawnId);
    expect(live.status).toBe("ACTIVE");

    const logoutCP = cpClient(cfg, logoutSession.accessToken);
    await logoutCP.createSession({
      spawnId: live.spawnId,
      transport: cpv1.SessionTransport.MOSH,
      runnable: "shell",
    });
    const acpSessionId = await waitForSession(
      cfg,
      logoutSession.accessToken,
      live.spawnId,
      cpv1.SessionTransport.ACP,
    );
    const moshSessionId = await waitForSession(
      cfg,
      logoutSession.accessToken,
      live.spawnId,
      cpv1.SessionTransport.MOSH,
    );
    const target = await logoutCP.getSpawnNodeKey({ spawnId: live.spawnId });
    expect(target.generation).not.toBe(0n);
    expect(target.targetNodeId).not.toBe("");
    const logoutJournalEpoch = Math.floor(Date.now() / 1000) - 1;
    const logoutACP = await bindSession(page, cfg, logoutSession, logoutSession.nodeAccessToken, live.spawnId, acpSessionId,
      target.generation, target.targetNodeId, "logout-acp");
    const logoutMOSH = await bindSession(page, cfg, logoutSession, logoutSession.nodeAccessToken, live.spawnId, moshSessionId,
      target.generation, target.targetNodeId, "logout-mosh");

    const predecessorNodeToken = logoutSession.nodeAccessToken;
    logoutSession = await refreshOAuthSession({ asOrigin: cfg.asOrigin }, logoutSession);
    expect(logoutSession.nodeAccessToken).not.toBe(predecessorNodeToken);
    await reauthenticateSocket(page, logoutSession, live.spawnId, acpSessionId,
      target.generation, target.targetNodeId, "logout-acp");
    await reauthenticateSocket(page, logoutSession, live.spawnId, moshSessionId,
      target.generation, target.targetNodeId, "logout-mosh");
    await new Promise((resolve) => setTimeout(resolve, 2_000));
    await expectSocketsOpen(page, ["logout-acp", "logout-mosh"]);
    const afterReauth = await cpClient(cfg, logoutSession.accessToken).listSessions({ spawnId: live.spawnId });
    expect(afterReauth.sessions.filter((item) => item.status === "active").map((item) => item.sessionId))
      .toEqual(expect.arrayContaining([acpSessionId, moshSessionId]));

    const logout = await fetch(`${cfg.asOrigin}/logout`, {
      method: "POST",
      headers: { Cookie: `logout_session=${logoutSession.refreshTokenRaw}` },
    });
    expect(logout.status, await logout.text()).toBe(200);
    await expectSocketsClosed(page, ["logout-acp", "logout-mosh"], 30_000);
    const logoutCloseRecords = await Promise.all([
      expectAuthorizationClosed(cfg, logoutJournalEpoch, {
        spawnId: live.spawnId,
        generation: Number(target.generation),
        sessionId: acpSessionId,
        clientId: logoutACP.clientId,
        reason: "node authorization revoked",
      }, 30_000),
      expectAuthorizationClosed(cfg, logoutJournalEpoch, {
        spawnId: live.spawnId,
        generation: Number(target.generation),
        sessionId: moshSessionId,
        clientId: logoutMOSH.clientId,
        reason: "node authorization revoked",
      }, 30_000),
    ]);
    expect(new Set(logoutCloseRecords.map((record) => record.attachmentId)).size).toBe(2);

    const expirySession = await establishCurrentSession(cfg);
    cleanupToken = expirySession.accessToken;
    const expiryCP = cpClient(cfg, expirySession.accessToken);
    const expiryTarget = await expiryCP.getSpawnNodeKey({ spawnId: live.spawnId });
    const signedExpiresAtMs = Number(decodeSessionArtifact(expirySession.nodeAccessToken).body.expiresAt) * 1000;
    expect(signedExpiresAtMs - Date.now()).toBeGreaterThan(14 * 60_000);
    const expiryJournalEpoch = Math.floor(Date.now() / 1000) - 1;
    const outageBinding = await bindSession(page, cfg, expirySession, expirySession.nodeAccessToken, live.spawnId, acpSessionId,
      expiryTarget.generation, expiryTarget.targetNodeId, "outage-expiry");

    await ssh(cfg, "sudo systemctl stop spawnery-authsvc");
    authsvcStopped = true;
    expect(await ssh(cfg, "sudo systemctl is-active spawnery-authsvc || true")).toBe("inactive");
    await expect(refreshOAuthSession({ asOrigin: cfg.asOrigin }, expirySession)).rejects.toThrow();
    await reauthenticateSocket(page, expirySession, live.spawnId, acpSessionId,
      expiryTarget.generation, expiryTarget.targetNodeId, "outage-expiry");
    await new Promise((resolve) => setTimeout(resolve, 2_000));
    await expectSocketsOpen(page, ["outage-expiry"]);

    await new Promise((resolve) => setTimeout(resolve, Math.max(0, signedExpiresAtMs + 1_000 - Date.now())));
    await expectSocketsClosed(page, ["outage-expiry"], 20_000);
    const expiryClose = await expectAuthorizationClosed(cfg, expiryJournalEpoch, {
      spawnId: live.spawnId,
      generation: Number(expiryTarget.generation),
      sessionId: acpSessionId,
      clientId: outageBinding.clientId,
      reason: "node authorization expired",
    }, 20_000);
    expect(expiryClose.timestampMs).toBeGreaterThanOrEqual(signedExpiresAtMs - 2_000);
    expect(expiryClose.timestampMs).toBeLessThanOrEqual(signedExpiresAtMs + 20_000);
  } catch (error) {
    testError = lifecycleFailure(error);
  } finally {
    if (authsvcStopped) {
      try {
        const refreshedCleanup = await restoreAuthService({
          start: () => ssh(cfg, "sudo systemctl start spawnery-authsvc").then(() => undefined),
          serviceState: () => ssh(cfg, "sudo systemctl is-active spawnery-authsvc || true"),
          freshSession: () => establishCurrentSession(cfg),
          wait: () => new Promise((resolve) => setTimeout(resolve, 500)),
        });
        cleanupToken = refreshedCleanup.accessToken;
        authsvcStopped = false;
      } catch (error) {
        restorationError = lifecycleFailure(error);
      }
    }
    if (cleanupToken) {
      const cleanup = cpClient(cfg, cleanupToken);
      cleanupErrors = await cleanupSpawnFailures(
        createdSpawnIds,
        (spawnId) => cleanup.deleteSpawn({ spawnId }),
      );
    }
  }
  const failure = aggregateLifecycleFailures(testError, restorationError, cleanupErrors);
  if (failure) throw failure;
});
