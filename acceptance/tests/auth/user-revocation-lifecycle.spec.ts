import { expect, test, type Page } from "@playwright/test";
import {
  buildSessionOpenSignedIntentB64,
  cpv1,
  exportSpkiDer,
  WebCryptoSessionSigner,
} from "@spawnery/client";
import {
  cpClient,
  decodeSessionArtifact,
  establishCurrentSession,
  loadVMAuthConfig,
  mintShortLivedNodeToken,
  ssh,
  submitSpawn,
  type VMAuthConfig,
} from "./root-anchored-artifacts";

const SOCKETS_KEY = "__spawneryRevocationSockets";

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
): Promise<void> {
  const intent = await buildSessionOpenSignedIntentB64(
    spawnId,
    sessionId,
    generation,
    targetNodeId,
    new WebCryptoSessionSigner(session.privateKey, session.publicKey),
  );
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
      clientId: `acc-revocation-${name}-${crypto.randomUUID()}`,
      sessionId,
      cursor: 0,
      signedIntent: intent,
    },
  });
}

async function expectSocketsClosed(page: Page, names: string[], timeoutMs: number): Promise<void> {
  await expect.poll(async () => page.evaluate(({ socketsKey, socketNames }) => {
    const holder = window as typeof window & Record<string, Record<string, WebSocket>>;
    return socketNames.map((name) => holder[socketsKey]?.[name]?.readyState);
  }, { socketsKey: SOCKETS_KEY, socketNames: names }), { timeout: timeoutMs }).toEqual(
    names.map(() => 3),
  );
}

test("user-revocation-lifecycle: logout closes ACP and MOSH; AS outage cannot extend node expiry", async ({ page }) => {
  test.setTimeout(10 * 60_000);
  const cfg = loadVMAuthConfig();
  await page.goto(cfg.webOrigin);
  const createdSpawnIds: string[] = [];
  let authsvcStopped = false;
  let cleanupToken = cfg.devToken;

  try {
    const logoutSession = await establishCurrentSession(cfg);
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
    await bindSession(page, cfg, logoutSession, logoutSession.nodeAccessToken, live.spawnId, acpSessionId,
      target.generation, target.targetNodeId, "logout-acp");
    await bindSession(page, cfg, logoutSession, logoutSession.nodeAccessToken, live.spawnId, moshSessionId,
      target.generation, target.targetNodeId, "logout-mosh");

    const logout = await fetch(`${cfg.asOrigin}/logout`, {
      method: "POST",
      headers: { Cookie: `logout_session=${logoutSession.refreshTokenRaw}` },
    });
    expect(logout.status, await logout.text()).toBe(200);
    await expectSocketsClosed(page, ["logout-acp", "logout-mosh"], 30_000);

    const expirySession = await establishCurrentSession(cfg);
    cleanupToken = expirySession.accessToken;
    const spki = await exportSpkiDer(expirySession.publicKey);
    const expiryCP = cpClient(cfg, expirySession.accessToken);
    const expiryTarget = await expiryCP.getSpawnNodeKey({ spawnId: live.spawnId });
    const expiryAccountId = decodeSessionArtifact(expirySession.nodeAccessToken).body.accountId;
    const shortNodeToken = await mintShortLivedNodeToken(cfg, expiryAccountId, spki, 20);
    await bindSession(page, cfg, expirySession, shortNodeToken, live.spawnId, acpSessionId,
      expiryTarget.generation, expiryTarget.targetNodeId, "outage-expiry");

    await ssh(cfg, "sudo systemctl stop spawnery-authsvc");
    authsvcStopped = true;
    expect(await ssh(cfg, "sudo systemctl is-active spawnery-authsvc || true")).toBe("inactive");
    await expectSocketsClosed(page, ["outage-expiry"], 30_000);
  } finally {
    if (authsvcStopped) {
      await ssh(cfg, "sudo systemctl start spawnery-authsvc").catch(() => {});
    }
    const cleanup = cpClient(cfg, cleanupToken);
    await Promise.all(createdSpawnIds.map((spawnId) => cleanup.deleteSpawn({ spawnId }).catch(() => {})));
  }
});
