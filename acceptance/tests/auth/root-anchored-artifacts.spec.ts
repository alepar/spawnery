import { test, expect } from "@playwright/test";
import { exportSpkiDer } from "@spawnery/client";
import {
  chainHashes,
  cpClient,
  decodeSessionArtifact,
  deployCurrentRevocation,
  establishCurrentSession,
  expectCPRejected,
  loadVMAuthConfig,
  mintVMToken,
  nodeLeafArtifact,
  ssh,
  submitSpawn,
} from "./root-anchored-artifacts";

test("root-anchored-artifacts: CP and spawnlet enforce root, purpose, audience, and signer revocation", async () => {
  test.setTimeout(8 * 60_000);
  const cfg = loadVMAuthConfig();
  const env = await ssh(cfg, "sudo cat /etc/spawnery/env.d/common.env");
  expect(env).toContain("CP_AUTH_ROOT_CA=/etc/spawnery/cp/root.pem");
  expect(env).toContain("NODE_ROOT_CA=/etc/spawnery/node/root.pem");
  expect(env).not.toMatch(/CP_AS_SESSION_PUBKEYS|NODE_AS_PUBKEYS|AS_SESSION_KEY_PEM|auth-signing-intermediate-key/);
  expect(await ssh(cfg, "sudo find /etc/spawnery -type f \\( -name '*session-pub*' -o -name 'auth-signing-intermediate-key.pem' \\)"), "no raw signer pin or offline issuer key in runtime tree").toBe("");

  const session = await establishCurrentSession(cfg);
  const cpArtifact = decodeSessionArtifact(session.accessToken);
  expect(cpArtifact.body.audience).toBe("cp");
  await expect(cpClient(cfg, session.accessToken).listSpawns({})).resolves.toBeDefined();

  const keyPair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]) as CryptoKeyPair;
  const spki = await exportSpkiDer(keyPair.publicKey);
  const nodeToken = await mintVMToken(cfg, "current", "node", spki);
  expect(decodeSessionArtifact(nodeToken).body.audience).toBe("node");
  expect(chainHashes(nodeToken)).toEqual(chainHashes(session.accessToken));
  await expectCPRejected(cfg, nodeToken);

  const createdSpawnIds: string[] = [];
  const cleanup = cpClient(cfg, cfg.devToken);
  const removeTerminal = async (spawnId: string) => {
    await cleanup.deleteSpawn({ spawnId });
    createdSpawnIds.splice(createdSpawnIds.indexOf(spawnId), 1);
  };
  try {
    const accepted = await submitSpawn(cfg, nodeToken, keyPair, "accepted");
    createdSpawnIds.push(accepted.spawnId);
    expect(accepted.status).toBe("ACTIVE");
    const wrongAudience = await submitSpawn(cfg, session.accessToken, keyPair, "wrong-audience");
    createdSpawnIds.push(wrongAudience.spawnId);
    expect(wrongAudience.status).toBe("ERROR");
    expect(wrongAudience.errorDetail).toContain("WRONG_AUDIENCE");
    await removeTerminal(wrongAudience.spawnId);

    const legacy = `${Buffer.from("legacy").toString("base64url")}.${Buffer.alloc(64).toString("base64url")}`;
    await expectCPRejected(cfg, legacy);
    const legacySpawn = await submitSpawn(cfg, legacy, keyPair, "legacy");
    createdSpawnIds.push(legacySpawn.spawnId);
    expect(legacySpawn.status).toBe("ERROR");
    expect(legacySpawn.errorDetail).toContain("TOKEN_INVALID");
    await removeTerminal(legacySpawn.spawnId);

    const nodeLeaf = await nodeLeafArtifact(cfg, "node", spki);
    await expectCPRejected(cfg, nodeLeaf);
    const nodeLeafSpawn = await submitSpawn(cfg, nodeLeaf, keyPair, "node-leaf");
    createdSpawnIds.push(nodeLeafSpawn.spawnId);
    expect(nodeLeafSpawn.status).toBe("ERROR");
    expect(nodeLeafSpawn.errorDetail).toContain("TOKEN_INVALID");
    await removeTerminal(nodeLeafSpawn.spawnId);

    await deployCurrentRevocation(cfg, 1);
    await expectCPRejected(cfg, session.accessToken);
    const revokedSpawn = await submitSpawn(cfg, nodeToken, keyPair, "revoked");
    createdSpawnIds.push(revokedSpawn.spawnId);
    expect(revokedSpawn.status).toBe("ERROR");
    expect(revokedSpawn.errorDetail).toContain("TOKEN_INVALID");
    await removeTerminal(revokedSpawn.spawnId);

    const nextCP = await mintVMToken(cfg, "next", "cp", spki);
    await expect(cpClient(cfg, nextCP).listSpawns({})).resolves.toBeDefined();
    const nextNode = await mintVMToken(cfg, "next", "node", spki);
    const replacement = await submitSpawn(cfg, nextNode, keyPair, "replacement");
    createdSpawnIds.push(replacement.spawnId);
    expect(replacement.status).toBe("ACTIVE");
  } finally {
    await Promise.all(createdSpawnIds.map((spawnId) => cleanup.deleteSpawn({ spawnId }).catch(() => {})));
  }
});
