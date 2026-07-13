import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { verifyNodeCertificateRevocation } from "./crl";

interface Bundle {
  class: "cloud" | "self-hosted";
  issuerPEM: string;
  crlPEM: string;
}

interface Scenario {
  chainPEM: string;
  bundles: Bundle[];
  trustDomain?: string;
}

interface RejectedScenario extends Scenario {
  error: string;
  goOutcome?: "parse-rejected" | "verify-rejected";
}

interface Vectors {
  now: string;
  rootPEM: string;
  chainPEM: string;
  validBundles: Bundle[];
  acceptedScenarios: Record<string, Scenario>;
  scenarios: Record<string, RejectedScenario>;
}

const vectors = JSON.parse(fs.readFileSync(path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "testdata/node-crl-vectors.json",
), "utf8")) as Vectors;
const now = new Date(vectors.now);
const trustDomain = "prod.spawnery.internal";
const GO_OUTCOME_SCENARIOS = [
  "outer-indefinite-length",
  "outer-noncanonical-length",
  "noncanonical-time",
  "absent-version",
  "non-v2-version",
  "algorithm-identifier-mismatch",
] as const;

function pem(type: string, der: Uint8Array): string {
  const base64 = btoa(String.fromCharCode(...der));
  return `-----BEGIN ${type}-----\n${base64}\n-----END ${type}-----\n`;
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
  it("accepts a current canonical CRL that does not revoke the leaf", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.validBundles,
      now,
      trustDomain,
    )).resolves.toBeUndefined();
  });

  it.each(Object.entries(vectors.acceptedScenarios))(
    "accepts the Go-approved signed %s scenario",
    async (_name, scenario) => {
      await expect(verifyNodeCertificateRevocation(
        scenario.chainPEM,
        vectors.rootPEM,
        scenario.bundles,
        now,
        scenario.trustDomain ?? trustDomain,
      )).resolves.toBeUndefined();
    },
  );

  it("fails when the chain issuer has no stamped bundle", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      vectors.validBundles.filter((bundle) => bundle.class === "cloud"),
      now,
      trustDomain,
    )).rejects.toThrow("exactly one stamped issuer");
  });

  it("fails when more than one stamped bundle contains the chain issuer", async () => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      [...vectors.validBundles, vectors.validBundles[1]],
      now,
      trustDomain,
    )).rejects.toThrow("exactly one stamped issuer");
  });

  it.each([
    ["issuer certificate", replaceSelfHosted(vectors.validBundles, {
      issuerPEM: pem("CERTIFICATE", new Uint8Array([1, 2, 3])),
    })],
    ["CRL", replaceSelfHosted(vectors.validBundles, {
      crlPEM: pem("X509 CRL", new Uint8Array([1, 2, 3])),
    })],
  ])("fails on malformed %s PEM/DER", async (_name, bundles) => {
    await expect(verifyNodeCertificateRevocation(
      vectors.chainPEM,
      vectors.rootPEM,
      bundles,
      now,
      trustDomain,
    )).rejects.toThrow();
  });

  it.each(Object.entries(vectors.scenarios))(
    "rejects the signed %s scenario",
    async (_name, scenario) => {
      await expect(verifyNodeCertificateRevocation(
        scenario.chainPEM,
        vectors.rootPEM,
        scenario.bundles,
        now,
        scenario.trustDomain ?? trustDomain,
      )).rejects.toThrow(scenario.error);
    },
  );

  it.each(GO_OUTCOME_SCENARIOS)("records Go rejection for %s", (name) => {
    expect(["parse-rejected", "verify-rejected"]).toContain(vectors.scenarios[name].goOutcome);
  });
});
