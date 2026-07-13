import { test, expect } from "@playwright/test";
import { createHash, X509Certificate } from "node:crypto";
import { exportSpkiDer } from "@spawnery/client";
import {
  assertDisposableVM,
  chainHashes,
  cpClient,
  decodeSessionArtifact,
  deployCurrentRevocation,
  establishCurrentSession,
  expectCPRejected,
  loadDestructiveVMAuthConfig,
  mintVMToken,
  nodeLeafArtifact,
  runtimeRootFingerprints,
  setCPAuthMode,
  ssh,
  submitSpawn,
} from "./root-anchored-artifacts";
import { generateNodeTrustFixtures } from "./node-trust-fixtures";

function certificateChain(pem: string): X509Certificate[] {
  const blocks = pem.match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g) ?? [];
  return blocks.map((block) => new X509Certificate(block));
}

test("root-anchored-artifacts: CP and spawnlet enforce root, purpose, audience, and signer revocation", async () => {
  test.setTimeout(8 * 60_000);
  const cfg = loadDestructiveVMAuthConfig();
  await assertDisposableVM(cfg);
  const env = await ssh(cfg, "sudo cat /etc/spawnery/env.d/common.env");
  expect(env).toContain("CP_AUTH_ROOT_CA=/etc/spawnery/cp/root.pem");
  expect(env).toContain("NODE_ROOT_CA=/etc/spawnery/node/root.pem");
  const roots = await runtimeRootFingerprints(cfg);
  expect(roots).toHaveLength(3);
  expect(roots.every((fingerprint) => /^[0-9a-f]{64}$/.test(fingerprint))).toBe(true);
  expect(new Set(roots).size, "AS, CP, and node must pin identical root certificate bytes").toBe(1);
  expect(env).not.toMatch(/CP_AS_SESSION_PUBKEYS|NODE_AS_PUBKEYS|AS_SESSION_KEY_PEM|auth-signing-intermediate-key/);
  expect(await ssh(cfg, "sudo find /etc/spawnery -type f \\( -name '*session-pub*' -o -name 'auth-signing-intermediate-key.pem' \\)"), "no raw signer pin or offline issuer key in runtime tree").toBe("");

  const trustFixtures = await generateNodeTrustFixtures(cfg);
  const pinnedRoot = new X509Certificate(await ssh(cfg, "sudo cat /etc/spawnery/node/root.pem"));
  const foreign = certificateChain(trustFixtures.foreignRootChainPEM);
  const unstamped = certificateChain(trustFixtures.unstampedIssuerChainPEM);
  expect(foreign).toHaveLength(2);
  expect(unstamped).toHaveLength(2);
  expect(foreign[0].verify(foreign[1].publicKey), "foreign-root leaf must be structurally valid").toBe(true);
  expect(foreign[1].verify(pinnedRoot.publicKey), "foreign issuer must not chain to the pinned root").toBe(false);
  expect(unstamped[0].verify(unstamped[1].publicKey), "unstamped leaf must be structurally valid").toBe(true);
  expect(unstamped[1].verify(pinnedRoot.publicKey), "unstamped issuer must chain to the pinned root").toBe(true);
  expect(trustFixtures.expiredCRLNextUpdateMs).toBeLessThanOrEqual(Date.now());

  const session = await establishCurrentSession(cfg);
  const cpArtifact = decodeSessionArtifact(session.accessToken);
  expect(cpArtifact.body.audience).toBe("cp");
  await expect(cpClient(cfg, session.accessToken).listSpawns({})).resolves.toBeDefined();

  const keyPair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]) as CryptoKeyPair;
  const spki = await exportSpkiDer(keyPair.publicKey);
  const nodeToken = await mintVMToken(cfg, "current", "node", spki, cpArtifact.body.accountId);
  const nodeArtifact = decodeSessionArtifact(nodeToken);
  expect(nodeArtifact.body.audience).toBe("node");
  expect(nodeArtifact.body.accountId).toBe(cpArtifact.body.accountId);
  expect(chainHashes(nodeToken)).toEqual(chainHashes(session.accessToken));
  await expectCPRejected(cfg, nodeToken);

  const createdSpawnIds: string[] = [];
  let cleanup = cpClient(cfg, session.accessToken);
  const removeTerminal = async (spawnId: string) => {
    await cleanup.deleteSpawn({ spawnId });
    createdSpawnIds.splice(createdSpawnIds.indexOf(spawnId), 1);
  };
  try {
    const accepted = await submitSpawn(cfg, nodeToken, keyPair, "accepted", session.accessToken);
    createdSpawnIds.push(accepted.spawnId);
    expect(accepted.status).toBe("ACTIVE");
    const wrongAudience = await submitSpawn(cfg, session.accessToken, keyPair, "wrong-audience", session.accessToken);
    createdSpawnIds.push(wrongAudience.spawnId);
    expect(wrongAudience.status).toBe("ERROR");
    expect(wrongAudience.errorDetail).toContain("WRONG_AUDIENCE");
    await removeTerminal(wrongAudience.spawnId);

    const legacy = `${Buffer.from("legacy").toString("base64url")}.${Buffer.alloc(64).toString("base64url")}`;
    await expectCPRejected(cfg, legacy);
    const legacySpawn = await submitSpawn(cfg, legacy, keyPair, "legacy", session.accessToken);
    createdSpawnIds.push(legacySpawn.spawnId);
    expect(legacySpawn.status).toBe("ERROR");
    expect(legacySpawn.errorDetail).toContain("TOKEN_INVALID");
    await removeTerminal(legacySpawn.spawnId);

    const nodeLeaf = await nodeLeafArtifact(cfg, "node", spki);
    const nodeLeafDecoded = decodeSessionArtifact(nodeLeaf);
    const nodeLeafCert = new X509Certificate(nodeLeafDecoded.envelope.signerChain[0]);
    const nodeLeafSPKI = nodeLeafCert.publicKey.export({ format: "der", type: "spki" });
    const nodeLeafKeyId = createHash("sha256").update(nodeLeafSPKI).digest();
    expect(Buffer.from(nodeLeafDecoded.envelope.keyId)).toEqual(nodeLeafKeyId);
    expect(nodeLeafDecoded.body.keyId).toBe(nodeLeafKeyId.toString("hex"));
    await expectCPRejected(cfg, nodeLeaf);
    const nodeLeafSpawn = await submitSpawn(cfg, nodeLeaf, keyPair, "node-leaf", session.accessToken);
    createdSpawnIds.push(nodeLeafSpawn.spawnId);
    expect(nodeLeafSpawn.status).toBe("ERROR");
    expect(nodeLeafSpawn.errorDetail).toContain("TOKEN_INVALID");
    expect(nodeLeafSpawn.errorDetail).toContain("signer issuer lacks auth-signing intermediate policy");
    expect(nodeLeafSpawn.errorDetail).not.toContain("unknown key_id");
    await removeTerminal(nodeLeafSpawn.spawnId);

    await deployCurrentRevocation(cfg, 1);
    await expectCPRejected(cfg, session.accessToken);
    try {
      await setCPAuthMode(cfg, "dev");
      cleanup = cpClient(cfg, cfg.destructiveDevToken);
      const revokedSpawn = await submitSpawn(cfg, nodeToken, keyPair, "revoked", cfg.destructiveDevToken);
      createdSpawnIds.push(revokedSpawn.spawnId);
      expect(revokedSpawn.status).toBe("ERROR");
      expect(revokedSpawn.errorDetail).toContain("TOKEN_INVALID");
      await removeTerminal(revokedSpawn.spawnId);
    } finally {
      await setCPAuthMode(cfg, "prod");
    }

    const nextCP = await mintVMToken(cfg, "next", "cp", spki, cfg.owner);
    cleanup = cpClient(cfg, nextCP);
    await expect(cpClient(cfg, nextCP).listSpawns({})).resolves.toBeDefined();
    const nextNode = await mintVMToken(cfg, "next", "node", spki, cfg.owner);
    const replacement = await submitSpawn(cfg, nextNode, keyPair, "replacement", nextCP);
    createdSpawnIds.push(replacement.spawnId);
    expect(replacement.status).toBe("ACTIVE");
  } finally {
    await Promise.all(createdSpawnIds.map((spawnId) => cleanup.deleteSpawn({ spawnId }).catch(() => {})));
  }
});
