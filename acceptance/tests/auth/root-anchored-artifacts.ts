import { execFile } from "node:child_process";
import { createHash, createPrivateKey, randomBytes, sign as nodeSign, X509Certificate } from "node:crypto";
import { promisify } from "node:util";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  authv1,
  buildIntentBodyBytes,
  buildSignedIntent,
  cpv1,
  createTransport,
  WebCryptoSessionSigner,
  toBase64Url,
} from "@spawnery/client";
import { establishOAuthSession } from "../../src/auth/oauth-session";

const execFileP = promisify(execFile);

export interface VMAuthConfig {
  ip: string;
  sshKey: string;
  sshUser: string;
  cpEndpoint: string;
  asOrigin: string;
  webOrigin: string;
  appId: string;
  model: string;
  destructiveDevToken: string;
  owner: string;
}

export function loadVMAuthConfig(env: NodeJS.ProcessEnv = process.env): VMAuthConfig {
  const required = (name: string): string => {
    const value = env[name];
    if (!value) throw new Error(`root-anchored-artifacts requires ${name}`);
    return value;
  };
  const [, owner] = required("ACC_IDENTITY_POOL").split(",", 1)[0].split("=", 2);
  return {
    ip: required("ACC_E2E_VM_IP"),
    sshKey: required("ACC_E2E_SSH_KEY"),
    sshUser: required("ACC_E2E_SSH_USER"),
    cpEndpoint: required("ACC_CP_ENDPOINT"),
    asOrigin: env.ACC_AS_ORIGIN || required("ACC_WEB_ORIGIN"),
    webOrigin: required("ACC_WEB_ORIGIN"),
    appId: required("ACC_TEST_APP_ID"),
    model: required("ACC_TEST_MODEL"),
    destructiveDevToken: required("ACC_DESTRUCTIVE_DEV_TOKEN"),
    owner,
  };
}

export async function ssh(cfg: VMAuthConfig, command: string): Promise<string> {
  const { stdout } = await execFileP("ssh", [
    "-i", cfg.sshKey,
    "-o", "BatchMode=yes",
    "-o", "StrictHostKeyChecking=no",
    `${cfg.sshUser}@${cfg.ip}`,
    command,
  ], { maxBuffer: 4 * 1024 * 1024 });
  return stdout.trim();
}

export function posixShellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

function remoteArgv(...argv: Array<string | number>): string {
  return argv.map((arg) => posixShellQuote(String(arg))).join(" ");
}

export function decodeSessionArtifact(wire: string) {
  const envelope = fromBinary(authv1.SignedAuthArtifactSchema, Buffer.from(wire, "base64url"));
  const body = fromBinary(authv1.SessionTokenBodySchema, envelope.payload);
  return { envelope, body };
}

export function chainHashes(wire: string): string[] {
  return decodeSessionArtifact(wire).envelope.signerChain.map((der) => createHash("sha256").update(der).digest("hex"));
}

export async function runtimeRootFingerprints(cfg: VMAuthConfig): Promise<string[]> {
  const output = await ssh(cfg, "sudo sha256sum /etc/spawnery/authsvc/root.pem /etc/spawnery/cp/root.pem /etc/spawnery/node/root.pem");
  return output.split("\n").map((line) => line.trim().split(/\s+/, 1)[0]);
}

export async function establishCurrentSession(cfg: VMAuthConfig) {
  return establishOAuthSession({
    asOrigin: cfg.asOrigin,
    redirectUri: `${cfg.webOrigin}/callback`,
    loginHint: cfg.owner,
  });
}

export async function mintVMToken(
  cfg: VMAuthConfig,
  signer: "current" | "next",
  audience: "cp" | "node",
  spki: Uint8Array,
  accountId: string,
): Promise<string> {
  const base = "/etc/spawnery/authsvc";
  return ssh(cfg, remoteArgv(
    "sudo", "/usr/local/bin/spawnery-ca", "auth-token", `${base}/root.pem`,
    `${base}/auth-signer-${signer}-key.pem`, `${base}/auth-signer-${signer}-chain.pem`,
    "prod", audience, accountId, Buffer.from(spki).toString("base64"),
  ));
}

export async function mintShortLivedNodeToken(
  cfg: VMAuthConfig,
  accountId: string,
  spki: Uint8Array,
  lifetimeSeconds: number,
): Promise<string> {
  if (lifetimeSeconds < 2 || lifetimeSeconds > 60) {
    throw new Error(`short-lived node token lifetime must be between 2 and 60 seconds, got ${lifetimeSeconds}`);
  }
  const read = async (name: string) => Buffer.from(
    await ssh(cfg, remoteArgv("sudo", "base64", "-w0", `/etc/spawnery/authsvc/${name}`)),
    "base64",
  );
  const keyPEM = await read("auth-signer-current-key.pem");
  const chainPEM = (await read("auth-signer-current-chain.pem")).toString("utf8");
  const certPEMs = chainPEM.match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g);
  if (!certPEMs?.length) throw new Error("deployed auth signer chain contained no certificates");
  const chain = certPEMs.map((pem) => new X509Certificate(pem));
  const keyId = createHash("sha256").update(chain[0].publicKey.export({ format: "der", type: "spki" })).digest();
  const now = Math.floor(Date.now() / 1000);
  const payload = toBinary(authv1.SessionTokenBodySchema, create(authv1.SessionTokenBodySchema, {
    accountId,
    tokenId: randomBytes(16).toString("hex"),
    audience: "node",
    issuedAt: BigInt(now),
    expiresAt: BigInt(now + lifetimeSeconds),
    sessionKeyHash: createHash("sha256").update(spki).digest(),
    keyId: keyId.toString("hex"),
  }));
  const message = Buffer.concat([Buffer.from("spawnery/session-token/v1"), payload]);
  const signature = nodeSign(null, message, createPrivateKey(keyPEM));
  return toBase64Url(toBinary(authv1.SignedAuthArtifactSchema, create(authv1.SignedAuthArtifactSchema, {
    artifactType: "session-token",
    payload,
    signature,
    signerChain: chain.map((cert) => cert.raw),
    keyId,
  })));
}

export function cpClient(cfg: VMAuthConfig, bearer: string) {
  return createClient(cpv1.SpawnService, createTransport({
    baseUrl: cfg.cpEndpoint,
    auth: { getBearer: async () => bearer },
  }));
}

export async function expectCPRejected(cfg: VMAuthConfig, wire: string): Promise<void> {
  try {
    await cpClient(cfg, wire).listSpawns({});
  } catch (error) {
    if (error instanceof ConnectError && error.code === Code.Unauthenticated) return;
    throw error;
  }
  throw new Error("CP accepted a token that must be unauthenticated");
}

export async function submitSpawn(
  cfg: VMAuthConfig,
  nodeToken: string,
  keyPair: CryptoKeyPair,
  suffix: string,
  cpToken: string,
): Promise<{ spawnId: string; status: string; errorDetail: string }> {
  const client = cpClient(cfg, cpToken);
  const created = await client.createSpawn({ appId: cfg.appId, model: cfg.model, name: `acc-root-${suffix}` });
  const spawnId = created.spawnId;
  let pending: cpv1.PendingIntent | undefined;
  for (let i = 0; i < 60; i++) {
    const response = await client.getPendingIntent({ spawnId });
    if (response.ready && response.pending) {
      pending = response.pending;
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  if (!pending) throw new Error(`pending intent not ready for ${spawnId}`);
  const bodyBytes = buildIntentBodyBytes({
    jti: createHash("sha256").update(`${spawnId}-${suffix}`).digest("hex").slice(0, 32),
    issuedAt: Math.floor(Date.now() / 1000),
    spawnId: pending.spawnId,
    generation: pending.generation,
    targetNodeId: pending.targetNodeId,
    op: pending.op,
    appRef: pending.appRef,
    image: pending.image,
    model: pending.model,
    dataRef: pending.dataRef,
    sessionId: "",
    mounts: pending.mounts,
  });
  const intent = await buildSignedIntent(
    pending.op,
    bodyBytes,
    new WebCryptoSessionSigner(keyPair.privateKey, keyPair.publicKey),
  );
  await client.submitIntent({ spawnId, intent, nodeAccessToken: nodeToken });

  let status = "UNSPECIFIED";
  let errorDetail = "";
  for (let i = 0; i < 120; i++) {
    const found = (await client.listSpawns({})).spawns.find((spawn) => spawn.spawnId === spawnId);
    if (found) {
      status = cpv1.SpawnStatus[found.status];
      errorDetail = found.errorDetail;
    }
    if (status === "ACTIVE" || status === "ERROR") break;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  return { spawnId, status, errorDetail };
}

export function cpAuthModePlan(
  cfg: Pick<VMAuthConfig, "destructiveDevToken" | "owner">,
  mode: "prod" | "dev",
): { configureCommand: string; expectedLog: string } {
  const envPath = "/etc/spawnery/env.d/zz-destructive.env";
  const dropInDir = "/etc/systemd/system/spawnery-cp.service.d";
  const dropInPath = `${dropInDir}/zz-destructive.conf`;
  if (mode === "dev") {
    const content = `CP_AUTH_MODE=dev\nCP_DEV_TOKENS=${cfg.destructiveDevToken}=${cfg.owner}\n`;
    const dropIn = `[Service]\nEnvironmentFile=${envPath}\n`;
    return {
      configureCommand: `install -d ${dropInDir}; printf %s ${posixShellQuote(content)} > ${envPath}; printf %s ${posixShellQuote(dropIn)} > ${dropInPath}`,
      expectedLog: "cp: auth mode=dev",
    };
  }
  return {
    configureCommand: `rm -f ${envPath} ${dropInPath}`,
    expectedLog: "cp: auth mode=prod",
  };
}

export function cpAuthModeReadinessCommand(expectedLog: string): string {
  return "sudo systemctl is-active spawnery-cp >/dev/null && "
    + "pid=$(sudo systemctl show --property MainPID --value spawnery-cp); "
    + "test \"$pid\" -gt 0 && "
    + "logs=$(sudo journalctl _SYSTEMD_UNIT=spawnery-cp.service _PID=\"$pid\" --no-pager); "
    + `printf %s "$logs" | grep -Fq ${posixShellQuote(expectedLog)} && `
    + `printf %s "$logs" | grep -Fq 'node connected' && echo ready`;
}

export async function setCPAuthMode(cfg: VMAuthConfig, mode: "prod" | "dev"): Promise<void> {
  const plan = cpAuthModePlan(cfg, mode);
  await ssh(cfg, remoteArgv("sudo", "sh", "-c", plan.configureCommand));
  await ssh(cfg, "sudo systemctl daemon-reload && sudo systemctl restart spawnery-cp");
  for (let i = 0; i < 60; i++) {
    const ready = await ssh(cfg, cpAuthModeReadinessCommand(plan.expectedLog)).catch(() => "");
    if (ready === "ready") return;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`CP did not report ${mode} auth mode after restart`);
}

export async function nodeLeafArtifact(cfg: VMAuthConfig, audience: "cp" | "node", spki: Uint8Array): Promise<string> {
  const read = async (name: string) => Buffer.from(await ssh(cfg, remoteArgv("sudo", "base64", "-w0", `/etc/spawnery/node/${name}`)), "base64");
  const keyPEM = await read("key.pem");
  const certPEM = await read("cert.pem");
  const chainPEM = await read("chain.pem");
  const leaf = new X509Certificate(certPEM);
  const leafSPKI = leaf.publicKey.export({ format: "der", type: "spki" });
  const keyId = createHash("sha256").update(leafSPKI).digest();
  const payload = toBinary(authv1.SessionTokenBodySchema, create(authv1.SessionTokenBodySchema, {
    accountId: cfg.owner,
    tokenId: "node-leaf-negative",
    audience,
    issuedAt: BigInt(Math.floor(Date.now() / 1000)),
    expiresAt: BigInt(Math.floor(Date.now() / 1000) + 900),
    sessionKeyHash: createHash("sha256").update(spki).digest(),
    keyId: keyId.toString("hex"),
  }));
  const message = Buffer.concat([Buffer.from("spawnery/session-token/v1"), payload]);
  const signature = nodeSign("sha256", message, { key: createPrivateKey(keyPEM), dsaEncoding: "ieee-p1363" });
  const certDER = leaf.raw;
  const chainDER = new X509Certificate(chainPEM).raw;
  return toBase64Url(toBinary(authv1.SignedAuthArtifactSchema, create(authv1.SignedAuthArtifactSchema, {
    artifactType: "session-token",
    payload,
    signature,
    signerChain: [certDER, chainDER],
    keyId,
  })));
}

export async function deployCurrentRevocation(cfg: VMAuthConfig, generation: number): Promise<void> {
  const restartEpoch = Math.floor(Date.now() / 1000) - 1;
  const wire = await ssh(cfg, remoteArgv(
    "sudo", "/usr/local/bin/spawnery-ca", "signer-revocation",
    "/var/lib/spawnery-offline/auth-signing-intermediate.pem",
    "/var/lib/spawnery-offline/auth-signing-intermediate-key.pem",
    "/etc/spawnery/authsvc/auth-signer-current-chain.pem", "prod", generation,
  ));
  const install = `printf '%s\\n' ${posixShellQuote(wire)} > /etc/spawnery/signer-revocations.artifact; grep -q ^CP_AUTH_SIGNER_REVOCATION_STATEMENT= /etc/spawnery/env.d/common.env || printf '%s\\n' 'CP_AUTH_SIGNER_REVOCATION_STATEMENT=/etc/spawnery/signer-revocations.artifact' >> /etc/spawnery/env.d/common.env; grep -q ^NODE_SIGNER_REVOCATION_STATEMENT= /etc/spawnery/env.d/common.env || printf '%s\\n' 'NODE_SIGNER_REVOCATION_STATEMENT=/etc/spawnery/signer-revocations.artifact' >> /etc/spawnery/env.d/common.env; systemctl restart spawnery-cp spawnery-node`;
  await ssh(cfg, remoteArgv("sudo", "sh", "-c", install));
  for (let i = 0; i < 60; i++) {
    const journal = remoteArgv("sudo", "journalctl", "-u", "spawnery-node", "--since", `@${restartEpoch}`, "--no-pager");
    const ready = await ssh(cfg, `curl -fsS http://127.0.0.1:8080/healthz >/dev/null && ${journal} | grep -q 'node: connected to CP' && echo ready`).catch(() => "");
    if (ready === "ready") return;
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  throw new Error("CP/node did not converge after signer revocation rollout");
}
