import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { verifyResolvedTarget } from "./target";

const vector = JSON.parse(fs.readFileSync(path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "testdata/node-crl-vectors.json",
), "utf8")) as {
  now: string;
  rootPEM: string;
  chainPEM: string;
  currentBundles: Array<{
    class: "cloud" | "self-hosted";
    issuerPEM: string;
    crlPEM: string;
  }>;
  revokingBundles: Array<{
    class: "cloud" | "self-hosted";
    issuerPEM: string;
    crlPEM: string;
  }>;
};

const target = {
  nodeCertChain: new TextEncoder().encode(vector.chainPEM),
  targetNodeId: "node-a",
  targetNodeClass: "self-hosted",
  targetNodeAccountId: "alice",
};
const pins = {
  rootCAPEM: vector.rootPEM,
  trustDomain: "prod.spawnery.internal",
  cloudAccountId: "cloud-system",
  nodeCRLs: vector.currentBundles,
};
const now = new Date(vector.now);

describe("verifyResolvedTarget", () => {
  it("rejects a revoked resolved target", async () => {
    const revokedTarget = {
      nodeCertChain: new TextEncoder().encode(vector.chainPEM),
      targetNodeId: "node-a",
      targetNodeClass: "self-hosted",
      targetNodeAccountId: "alice",
    };
    await expect(verifyResolvedTarget(revokedTarget, "alice", {
      rootCAPEM: vector.rootPEM,
      trustDomain: "prod.spawnery.internal",
      cloudAccountId: "cloud-system",
      nodeCRLs: vector.revokingBundles,
    }, now)).rejects.toThrow("revoked");
  });

  it("accepts a root-verified self-hosted node matching every typed field and logged-in account", async () => {
    await expect(verifyResolvedTarget(target, "alice", pins, now)).resolves.toBeUndefined();
  });

  it.each([
    ["node ID", { targetNodeId: "other" }],
    ["class", { targetNodeClass: "cloud" }],
    ["typed account", { targetNodeAccountId: "mallory" }],
  ])("rejects a %s substitution", async (_name, mutation) => {
    await expect(verifyResolvedTarget({ ...target, ...mutation }, "alice", pins, now)).rejects.toThrow();
  });

  it("rejects a self-hosted target belonging to another account", async () => {
    await expect(verifyResolvedTarget(target, "mallory", pins, now)).rejects.toThrow();
  });

  it.each([
    [{ ...pins, rootCAPEM: "" }],
    [{ ...pins, trustDomain: "" }],
    [{ ...pins, cloudAccountId: "" }],
  ])("fails closed when a trust pin is missing", async (badPins) => {
    await expect(verifyResolvedTarget(target, "alice", badPins, now)).rejects.toThrow();
  });

  it.each([
    [{ ...target, nodeCertChain: new Uint8Array() }],
    [{ ...target, targetNodeId: "" }],
    [{ ...target, targetNodeClass: "" }],
    [{ ...target, targetNodeAccountId: "" }],
  ])("fails closed when target metadata is missing", async (incomplete) => {
    await expect(verifyResolvedTarget(incomplete, "alice", pins, now)).rejects.toThrow();
  });

  it("requires cloud identity account to match the stamped cloud account", async () => {
    const leaf = { sanURIs: ["spiffe://prod.spawnery.internal/node/cloud/cloud-system/node-c"] };
    const deps = {
      verifyCertChain: async () => leaf,
      parseSPIFFEPrincipal: () => ({
        trustDomain: "prod.spawnery.internal", kind: "node" as const, role: "cloud" as const,
        accountId: "cloud-system", nodeId: "node-c",
      }),
      verifyNodeCertificateRevocation: async () => undefined,
    };
    const cloud = {
      nodeCertChain: new TextEncoder().encode("pem"), targetNodeId: "node-c",
      targetNodeClass: "cloud", targetNodeAccountId: "other-system",
    };
    await expect(verifyResolvedTarget(cloud, "alice", pins, now, deps)).rejects.toThrow();
  });
});
