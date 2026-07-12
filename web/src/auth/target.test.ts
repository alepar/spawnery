import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { verifyResolvedTarget } from "./target";

const vector = JSON.parse(fs.readFileSync(path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/secrets/subkey/testdata/subkey/verify_node.json",
), "utf8")) as {
  chain_pem: string;
  root_pem: string;
  expected_node_id: string;
  expected_account_id: string;
  expected_class: string;
  not_before: string;
};

const target = {
  nodeCertChain: new TextEncoder().encode(vector.chain_pem),
  targetNodeId: vector.expected_node_id,
  targetNodeClass: vector.expected_class,
  targetNodeAccountId: vector.expected_account_id,
};
const pins = {
  rootCAPEM: vector.root_pem,
  trustDomain: "prod.spawnery.internal",
  cloudAccountId: "cloud-system",
};
const now = new Date(new Date(vector.not_before).getTime() + 60 * 60 * 1000);

describe("verifyResolvedTarget", () => {
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
    };
    const cloud = {
      nodeCertChain: new TextEncoder().encode("pem"), targetNodeId: "node-c",
      targetNodeClass: "cloud", targetNodeAccountId: "other-system",
    };
    await expect(verifyResolvedTarget(cloud, "alice", pins, now, deps)).rejects.toThrow();
  });
});
