import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { fromBER } from "asn1js";
import { CertificateRevocationList } from "pkijs";
import { describe, expect, it } from "vitest";

import { verifyNodeCertificateRevocation } from "./crl";

interface Bundle {
  class: "cloud" | "self-hosted";
  issuerPEM: string;
  crlPEM: string;
}

interface Vectors {
  now: string;
  rootPEM: string;
  cloudIssuerPEM: string;
  selfHostedIssuerPEM: string;
  nodeLeafPEM: string;
  chainPEM: string;
  currentBundles: Bundle[];
  revokingBundles: Bundle[];
  futureBundles: Bundle[];
  expiredBundles: Bundle[];
  wrongIssuerBundles: Bundle[];
  badSignatureBundles: Bundle[];
}

const vectors = JSON.parse(fs.readFileSync(path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "testdata/node-crl-vectors.json",
), "utf8")) as Vectors;
const now = new Date(vectors.now);

function pemDER(pem: string): Uint8Array {
  const match = pem.match(/-----BEGIN [^-]+-----\r?\n([\s\S]+?)\r?\n-----END [^-]+-----/);
  if (!match) throw new Error("test PEM missing");
  return Uint8Array.from(atob(match[1].replace(/\s+/g, "")), (char) => char.charCodeAt(0));
}

function pem(type: string, der: Uint8Array): string {
  const base64 = btoa(String.fromCharCode(...der));
  return `-----BEGIN ${type}-----\n${base64}\n-----END ${type}-----`;
}

function withoutNextUpdate(source: string): string {
  const parsed = fromBER(new Uint8Array(pemDER(source)));
  if (parsed.offset === -1) throw new Error("fixture CRL failed to parse");
  const crl = new CertificateRevocationList({ schema: parsed.result });
  crl.nextUpdate = undefined;
  return pem("X509 CRL", new Uint8Array(crl.toSchema(true).toBER(false)));
}

function replaceSelfHosted(
  bundles: Bundle[],
  mutation: Partial<Pick<Bundle, "issuerPEM" | "crlPEM">>,
): Bundle[] {
  return bundles.map((bundle) => bundle.class === "self-hosted"
    ? { ...bundle, ...mutation }
    : bundle);
}

describe("verifyNodeCertificateRevocation", () => {
  it("accepts a current signed CRL that does not revoke the leaf", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.currentBundles,
      now,
    )).resolves.toBeUndefined();
  });

  it("fails when the chain issuer has no stamped bundle", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.currentBundles.filter((bundle) => bundle.class === "cloud"),
      now,
    )).rejects.toThrow("exactly one stamped issuer");
  });

  it("fails when more than one stamped bundle contains the chain issuer", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      [...vectors.currentBundles, vectors.currentBundles[1]],
      now,
    )).rejects.toThrow("exactly one stamped issuer");
  });

  it.each([
    ["issuer certificate", replaceSelfHosted(vectors.currentBundles, {
      issuerPEM: pem("CERTIFICATE", new Uint8Array([1, 2, 3])),
    })],
    ["CRL", replaceSelfHosted(vectors.currentBundles, {
      crlPEM: pem("X509 CRL", new Uint8Array([1, 2, 3])),
    })],
  ])("fails on malformed %s PEM/DER", async (_name, bundles) => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      bundles,
      now,
    )).rejects.toThrow();
  });

  it("fails when the stamped issuer is not byte-equal to the chain issuer", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      replaceSelfHosted(vectors.currentBundles, { issuerPEM: vectors.cloudIssuerPEM }),
      now,
    )).rejects.toThrow("exactly one stamped issuer");
  });

  it("fails when the chain issuer is not rooted in the stamped root", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.nodeLeafPEM,
      vectors.currentBundles,
      now,
    )).rejects.toThrow("not rooted");
  });

  it("fails when the CRL signature is invalid", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.badSignatureBundles,
      now,
    )).rejects.toThrow("signature");
  });

  it("fails when thisUpdate is later than the verification clock", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.futureBundles,
      now,
    )).rejects.toThrow("not current");
  });

  it("fails when nextUpdate is absent", async () => {
    const bundles = replaceSelfHosted(vectors.currentBundles, {
      crlPEM: withoutNextUpdate(vectors.currentBundles[1].crlPEM),
    });
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      bundles,
      now,
    )).rejects.toThrow("nextUpdate");
  });

  it("fails when nextUpdate is expired", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.expiredBundles,
      now,
    )).rejects.toThrow("expired");
  });

  it("fails when the CRL issuer RDN differs from the stamped issuer subject", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.wrongIssuerBundles,
      now,
    )).rejects.toThrow("issuer");
  });

  it("fails when the CRL revokes the leaf serial", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.revokingBundles,
      now,
    )).rejects.toThrow("revoked");
  });
});
