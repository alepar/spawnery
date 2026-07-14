import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { createKnownVMTargetVerifier } from "./oracle";

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

describe("createKnownVMTargetVerifier", () => {
  const verify = createKnownVMTargetVerifier({
    rootCAPEM: vector.root_pem,
    trustDomain: "prod.spawnery.internal",
    expectedNodeId: vector.expected_node_id,
    expectedNodeClass: vector.expected_class,
    expectedNodeAccountId: vector.expected_account_id,
    now: () => new Date(new Date(vector.not_before).getTime() + 60 * 60 * 1000),
  });

  it("accepts the root-verified node only when chain and typed identity are exact", async () => {
    await expect(verify(target)).resolves.toBeUndefined();
  });

  it.each([
    ["node id", { targetNodeId: "other" }],
    ["class", { targetNodeClass: "cloud" }],
    ["account", { targetNodeAccountId: "mallory" }],
    ["chain", { nodeCertChain: new TextEncoder().encode(vector.chain_pem.replace(/A/, "B")) }],
  ])("rejects %s substitution before signing", async (_name, mutation) => {
    await expect(verify({ ...target, ...mutation })).rejects.toThrow();
  });
});
