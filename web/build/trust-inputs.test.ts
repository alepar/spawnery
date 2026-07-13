import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import type { ConfigEnv } from "vite";
import { afterEach, describe, expect, it, vi } from "vitest";

import { validateTrustInputs } from "./trust-inputs";

const validNodeCRLs = JSON.stringify([
  {
    class: "cloud",
    issuerPEM: "-----BEGIN CERTIFICATE-----\ncloud-issuer\n-----END CERTIFICATE-----",
    crlPEM: "-----BEGIN X509 CRL-----\ncloud-crl\n-----END X509 CRL-----",
  },
  {
    class: "self-hosted",
    issuerPEM: "-----BEGIN CERTIFICATE-----\nself-hosted-issuer\n-----END CERTIFICATE-----",
    crlPEM: "-----BEGIN X509 CRL-----\nself-hosted-crl\n-----END X509 CRL-----",
  },
]);

const validEnv = {
  VITE_ROOT_CA_PEM: "-----BEGIN CERTIFICATE-----\nroot\n-----END CERTIFICATE-----",
  VITE_TRUST_DOMAIN: "prod.spawnery.internal",
  VITE_CLOUD_ACCOUNT_ID: "spawnery-system",
  VITE_NODE_CRLS_JSON: validNodeCRLs,
};

describe("validateTrustInputs", () => {
  it("accepts the complete release trust stamp", () => {
    expect(validateTrustInputs(validEnv, true)).toEqual({
      rootCAPEM: validEnv.VITE_ROOT_CA_PEM,
      trustDomain: validEnv.VITE_TRUST_DOMAIN,
      cloudAccountId: validEnv.VITE_CLOUD_ACCOUNT_ID,
      nodeCRLs: JSON.parse(validNodeCRLs),
    });
  });

  it.each([
    "VITE_ROOT_CA_PEM",
    "VITE_TRUST_DOMAIN",
    "VITE_CLOUD_ACCOUNT_ID",
    "VITE_NODE_CRLS_JSON",
  ] as const)("rejects missing or empty %s when trust inputs are required", (name) => {
    expect(() => validateTrustInputs({ ...validEnv, [name]: "" }, true)).toThrow(name);
    const { [name]: _omitted, ...withoutInput } = validEnv;
    expect(() => validateTrustInputs(withoutInput, true)).toThrow(name);
  });

  it.each([
    "VITE_ROOT_CA_PEM",
    "VITE_TRUST_DOMAIN",
    "VITE_CLOUD_ACCOUNT_ID",
    "VITE_NODE_CRLS_JSON",
  ] as const)("rejects placeholder %s", (name) => {
    expect(() =>
      validateTrustInputs({ ...validEnv, [name]: "not-configured-placeholder" }, true),
    ).toThrow(name);
  });

  it("rejects malformed node CRL JSON", () => {
    expect(() =>
      validateTrustInputs({ ...validEnv, VITE_NODE_CRLS_JSON: "not-json" }, true),
    ).toThrow("VITE_NODE_CRLS_JSON");
  });

  it("requires exactly one cloud and one self-hosted CRL", () => {
    const cloud = JSON.parse(validNodeCRLs)[0];
    expect(() =>
      validateTrustInputs(
        { ...validEnv, VITE_NODE_CRLS_JSON: JSON.stringify([cloud, cloud]) },
        true,
      ),
    ).toThrow("duplicate class");
    expect(() =>
      validateTrustInputs({ ...validEnv, VITE_NODE_CRLS_JSON: JSON.stringify([cloud]) }, true),
    ).toThrow("exactly");
  });

  it.each(["issuerPEM", "crlPEM"] as const)("rejects a CRL entry missing %s", (field) => {
    const entries = JSON.parse(validNodeCRLs);
    delete entries[0][field];
    expect(() =>
      validateTrustInputs({ ...validEnv, VITE_NODE_CRLS_JSON: JSON.stringify(entries) }, true),
    ).toThrow(field);
  });

  it("rejects malformed CRL entries instead of ignoring extra fields", () => {
    const entries = JSON.parse(validNodeCRLs);
    entries[0].unexpected = "value";
    expect(() =>
      validateTrustInputs({ ...validEnv, VITE_NODE_CRLS_JSON: JSON.stringify(entries) }, true),
    ).toThrow("exactly");
  });

  it("allows auth-disabled development without trust inputs", () => {
    expect(validateTrustInputs({}, false)).toEqual({
      rootCAPEM: "",
      trustDomain: "",
      cloudAccountId: "",
      nodeCRLs: [],
    });
  });
});

async function evaluateViteConfig(mode: string): Promise<unknown> {
  vi.resetModules();
  const { default: config } = await import("../vite.config");
  if (typeof config !== "function") throw new Error("Vite config does not validate inputs by mode");
  return config({ command: "build", mode } as ConfigEnv);
}

afterEach(() => vi.unstubAllEnvs());

describe("Vite trust input gate", () => {
  it("requires stamped trust inputs for production builds", async () => {
    vi.stubEnv("VITE_ROOT_CA_PEM", "");
    await expect(evaluateViteConfig("production")).rejects.toThrow("VITE_ROOT_CA_PEM");
  });

  it("requires stamped trust inputs when development auth is enabled", async () => {
    vi.stubEnv("VITE_AUTH_ENABLED", "1");
    vi.stubEnv("VITE_ROOT_CA_PEM", "");
    await expect(evaluateViteConfig("development")).rejects.toThrow("VITE_ROOT_CA_PEM");
  });

  it("allows auth-disabled development without stamped trust inputs", async () => {
    vi.stubEnv("VITE_AUTH_ENABLED", "");
    for (const name of Object.keys(validEnv)) vi.stubEnv(name, "");
    await expect(evaluateViteConfig("development")).resolves.toBeDefined();
  });
});

describe("trust stamp documentation", () => {
  const webDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

  it("documents every release trust variable in the environment template and Vite types", () => {
    const example = readFileSync(resolve(webDir, ".env.example"), "utf8");
    const viteTypes = readFileSync(resolve(webDir, "src/vite-env.d.ts"), "utf8");
    for (const name of [
      "VITE_AUTH_ENABLED",
      "VITE_ROOT_CA_PEM",
      "VITE_TRUST_DOMAIN",
      "VITE_CLOUD_ACCOUNT_ID",
      "VITE_NODE_CRLS_JSON",
    ]) {
      expect(example).toContain(name);
      expect(viteTypes).toContain(name);
    }
  });

  it("records the verified PKI package licenses", () => {
    const notices = readFileSync(resolve(webDir, "THIRD_PARTY_NOTICES.md"), "utf8");
    expect(notices).toContain("PKI.js");
    expect(notices).toContain("ASN.1.js");
    expect(notices.match(/BSD-3-Clause/g)).toHaveLength(2);
  });
});
