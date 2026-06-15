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
  type RevocationChecker,
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
  root_pem:              string;
  leaf_pem:              string;
  intermediate_pem:      string;
  chain_pem:             string;
  subkey_json:           string;
  expected_hpke_pub:     string;
  expected_node_id:      string;
  expected_account_id:   string;
  expected_class:        string;
  not_before:            string;
  not_after:             string;
  leaf_not_after:        string;  // leaf cert expiry — for cert-validity negative tests
  forged_cloud_chain_pem: string; // cloud-SAN leaf signed by SH intermediate
  non_ca_leaf_chain_pem:  string; // leaf cert used as CA to sign another leaf
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

  // verifyAt is 1 hour into the sub-key's validity window, within the cert validity too.
  const verifyAt = new Date(new Date(v.not_before).getTime() + 60 * 60 * 1000);

  it("verifyCertChain accepts the node leaf against the pinned root", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem, verifyAt);
    expect(leaf.sanDNS).toContain("nodes.spawnery.internal");
  });

  it("parseSANIdentity extracts correct identity from SAN", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem, verifyAt);
    const id = parseSANIdentity(leaf.sanDNS);
    expect(id.nodeId).toBe(v.expected_node_id);
    expect(id.accountId).toBe(v.expected_account_id);
    expect(id.nodeClass).toBe(v.expected_class);
  });

  it("verifySignedSubKey accepts valid sub-key", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem, verifyAt);
    const certPub = await importCertPubKey(leaf);
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
    await expect(verifySignedSubKey(sk, certPub, verifyAt)).resolves.toBeUndefined();
  });

  it("verifySignedSubKey rejects an expired sub-key", async () => {
    const leaf = await verifyCertChain(v.chain_pem, v.root_pem, verifyAt);
    const certPub = await importCertPubKey(leaf);
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
    const notAfter = new Date(v.not_after);
    const expired = new Date(notAfter.getTime() + 1000); // 1s past sub-key expiry
    await expect(verifySignedSubKey(sk, certPub, expired)).rejects.toThrow("expired");
  });

  it("verifyNodeForSealing returns expected HPKE pubkey + identity", async () => {
    const sk: SignedSubKey = JSON.parse(v.subkey_json);
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

  it("verifyNodeForSealing rejects missing cert chain when a root is pinned", async () => {
    await expect(
      verifyNodeForSealing(
        "",
        v.root_pem,
        v.subkey_json,
        { tenancy: "self-hosted", accountId: v.expected_account_id },
        verifyAt,
      ),
    ).rejects.toThrow("cert chain");
  });

  it("verifyNodeForSealing preserves explicit dev mode when no root is pinned", async () => {
    const result = await verifyNodeForSealing(
      "",
      "",
      v.subkey_json,
      { tenancy: "self-hosted", accountId: v.expected_account_id },
      verifyAt,
    );
    expect(result.identity.nodeId).toBe(v.expected_node_id);
    expect(result.identity.accountId).toBe("dev");
    expect(result.identity.nodeClass).toBe("cloud");
  });

  it("verifyNodeForSealing rejects wrong tenancy expectation", async () => {
    await expect(
      verifyNodeForSealing(v.chain_pem, v.root_pem, v.subkey_json, { tenancy: "cloud" }, verifyAt),
    ).rejects.toThrow("tenancy");
  });

  // WM8: revoked-node-refusal — delivery to a node with a revoked cert must fail closed.
  it("verifyNodeForSealing rejects a revoked node (WM8)", async () => {
    const revokedChecker: RevocationChecker = {
      async check(_nodeId: string): Promise<void> {
        throw new Error("node is on the AS revocation deny-list");
      },
    };
    await expect(
      verifyNodeForSealing(
        v.chain_pem,
        v.root_pem,
        v.subkey_json,
        { tenancy: "self-hosted", accountId: v.expected_account_id },
        verifyAt,
        revokedChecker,
      ),
    ).rejects.toThrow("revocation deny-list");
  });

  // WM8: revocation checker that errors (e.g., network failure) must also fail closed.
  it("verifyNodeForSealing fails closed when revocation check errors (WM8)", async () => {
    const errorChecker: RevocationChecker = {
      async check(_nodeId: string): Promise<void> {
        throw new Error("revocation list unavailable (network error)");
      },
    };
    await expect(
      verifyNodeForSealing(
        v.chain_pem,
        v.root_pem,
        v.subkey_json,
        { tenancy: "self-hosted", accountId: v.expected_account_id },
        verifyAt,
        errorChecker,
      ),
    ).rejects.toThrow("network error");
  });

  // SECURITY: name constraints — a cloud-SAN leaf signed by a self-hosted intermediate
  // must be rejected even though the signature chain is cryptographically valid.
  it("verifyCertChain rejects forged-cloud cert (name constraints, security)", async () => {
    await expect(
      verifyCertChain(v.forged_cloud_chain_pem, v.root_pem, verifyAt),
    ).rejects.toThrow("name constraints");
  });

  // SECURITY: non-CA intermediate — a chain where the intermediate lacks CA:TRUE must be rejected.
  it("verifyCertChain rejects chain with non-CA intermediate (basicConstraints, security)", async () => {
    await expect(
      verifyCertChain(v.non_ca_leaf_chain_pem, v.root_pem, verifyAt),
    ).rejects.toThrow("CA:TRUE");
  });

  // SECURITY: validity — a cert that has expired (notAfter in the past) must be rejected.
  it("verifyCertChain rejects an expired leaf cert (validity, security)", async () => {
    const leafNotAfter = new Date(v.leaf_not_after);
    const pastExpiry = new Date(leafNotAfter.getTime() + 1000); // 1s past leaf notAfter
    await expect(
      verifyCertChain(v.chain_pem, v.root_pem, pastExpiry),
    ).rejects.toThrow("expired");
  });
});
