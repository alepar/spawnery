import { execFile } from "node:child_process";
import { createHash, createPrivateKey, sign as nodeSign, X509Certificate } from "node:crypto";
import { promisify } from "node:util";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  authv1,
  buildIntentBodyBytes,
  buildSignedIntent,
  cpv1,
  createTransport,
  exportSpkiDer,
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
  devToken: string;
  owner: string;
}

export function loadVMAuthConfig(env: NodeJS.ProcessEnv = process.env): VMAuthConfig {
  const required = (name: string): string => {
    const value = env[name];
    if (!value) throw new Error(`root-anchored-artifacts requires ${name}`);
    return value;
  };
  const [devToken, owner] = required("ACC_IDENTITY_POOL").split(",", 1)[0].split("=", 2);
  return {
    ip: required("ACC_E2E_VM_IP"),
    sshKey: required("ACC_E2E_SSH_KEY"),
    sshUser: required("ACC_E2E_SSH_USER"),
    cpEndpoint: required("ACC_CP_ENDPOINT"),
    asOrigin: env.ACC_AS_ORIGIN || required("ACC_WEB_ORIGIN"),
    webOrigin: required("ACC_WEB_ORIGIN"),
    appId: required("ACC_TEST_APP_ID"),
    model: required("ACC_TEST_MODEL"),
    devToken,
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
): Promise<string> {
  const base = "/etc/spawnery/authsvc";
  return ssh(cfg, remoteArgv(
    "sudo", "/usr/local/bin/spawnery-ca", "auth-token", `${base}/root.pem`,
    `${base}/auth-signer-${signer}-key.pem`, `${base}/auth-signer-${signer}-chain.pem`,
    "prod", audience, cfg.owner, Buffer.from(spki).toString("base64"),
  ));
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
): Promise<{ spawnId: string; status: string; errorDetail: string }> {
  const client = cpClient(cfg, cfg.devToken);
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
  const spki = await exportSpkiDer(keyPair.publicKey);
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
  const intent = await buildSignedIntent(pending.op, bodyBytes, keyPair.privateKey, spki);
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
