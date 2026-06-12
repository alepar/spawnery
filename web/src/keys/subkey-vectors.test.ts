/**
 * Cross-language test vectors for subkey.verifyNodeForSealing.
 *
 * Reads internal/secrets/subkey/testdata/subkey/verify_node.json (generated
 * by internal/secrets/subkey/vectors_test.go -update-subkey) and validates:
 *   1. verifyCertChain succeeds against the pinned root.
 *   2. parseSANIdentity extracts nodeId/accountId/class correctly.
 *   3. verifySignedSubKey verifies the sub-key signature + expiry.
 *   4. verifyNodeForSealing returns the expected HPKE pubkey + identity.
 */
import { describe, it, expect } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  verifyNodeForSealing,
  verifySignedSubKey,
  type SignedSubKey,
} from "./subkey";
import {
  verifyCertChain,
  parseSANIdentity,
  importCertPubKey,
} from "./x509";

const VECTORS_FILE = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/secrets/subkey/testdata/subkey/verify_node.json",
);

interface SubKeyVector {
  root_pem:           string;
  leaf_pem:           string;
  intermediate_pem:   string;
  chain_pem:          string;
  subkey_json:        string;
  expected_hpke_pub:  string;
  expected_node_id:   string;
  expected_account_id: string;
  expected_class:     string;
  not_before:         string;
  not_after:          string;
}

function loadVectors(): SubKeyVector | null {
  try {
    return JSON.parse(fs.readFileSync(VECTORS_FILE, "utf-8")) as SubKeyVector;
  } catch {
    return null;
  }
}

function fromBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

describe("subkey Go-TS cross-language vectors", () => {
  const v = loadVectors();
  if (!v) {
    it.todo("vector file not found — run: go test ./internal/secrets/subkey/ -update-subkey");
    return;
  }

  it("verifyCertChain accepts the node leaf against the pinned root", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem);
    expect(leaf.sanDNS).toContain("nodes.spawnery.internal");
  });

  it("parseSANIdentity extracts correct identity from SAN", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem);
    const id = parseSANIdentity(leaf.sanDNS);
    expect(id.nodeId).toBe(v.expected_node_id);
    expect(id.accountId).toBe(v.expected_account_id);
    expect(id.nodeClass).toBe(v.expected_class);
  });

  it("verifySignedSubKey accepts valid sub-key", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem);
    const certPub = await importCertPubKey(leaf);
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
    // Use a time 1 hour into the validity window.
    const notBefore = new Date(v.not_before);
    const verifyAt = new Date(notBefore.getTime() + 60 * 60 * 1000);
    await expect(verifySignedSubKey(sk, certPub, verifyAt)).resolves.toBeUndefined();
  });

  it("verifySignedSubKey rejects an expired sub-key", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem);
    const certPub = await importCertPubKey(leaf);
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
    const notAfter = new Date(v.not_after);
    const expired = new Date(notAfter.getTime() + 1000); // 1s past expiry
    await expect(verifySignedSubKey(sk, certPub, expired)).rejects.toThrow("expired");
  });

  it("verifyNodeForSealing returns expected HPKE pubkey + identity", async () => {
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
    const notBefore = new Date(v.not_before);
    const verifyAt = new Date(notBefore.getTime() + 60 * 60 * 1000);
    const result = await verifyNodeForSealing(
      v.chain_pem,
      v.root_pem,
      v.subkey_json,
      { tenancy: "self-hosted", accountId: v.expected_account_id },
      verifyAt,
    );
    // Check returned HPKE pubkey matches expected.
    const expectedPub = fromBase64(v.expected_hpke_pub);
    expect(Array.from(result.hpkePub)).toEqual(Array.from(expectedPub));
    // Check identity.
    expect(result.identity.nodeId).toBe(v.expected_node_id);
    expect(result.identity.accountId).toBe(v.expected_account_id);
    expect(result.identity.nodeClass).toBe(v.expected_class);
    void sk; // ensure we parsed the JSON
  });

  it("verifyNodeForSealing rejects wrong tenancy expectation", async () => {
    const notBefore = new Date(v.not_before);
    const verifyAt = new Date(notBefore.getTime() + 60 * 60 * 1000);
    await expect(
      verifyNodeForSealing(v.chain_pem, v.root_pem, v.subkey_json, { tenancy: "cloud" }, verifyAt),
    ).rejects.toThrow("tenancy");
  });
});
