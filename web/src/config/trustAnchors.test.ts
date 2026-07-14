import { afterEach, describe, expect, it, vi } from "vitest";

const nodeCRLs = [
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
] as const;

const valid = {
  VITE_AUTH_ENABLED: "1",
  VITE_ROOT_CA_PEM: "-----BEGIN CERTIFICATE-----\nreal-root\n-----END CERTIFICATE-----",
  VITE_TRUST_DOMAIN: "prod.spawnery.internal",
  VITE_CLOUD_ACCOUNT_ID: "spawnery-system",
  VITE_NODE_CRL_BUNDLE_JSON: JSON.stringify(nodeCRLs),
};

async function load(overrides: Record<string, string> = {}) {
  vi.resetModules();
  for (const [name, value] of Object.entries({ ...valid, ...overrides })) vi.stubEnv(name, value);
  return import("./trustAnchors");
}

afterEach(() => vi.unstubAllEnvs());

describe("release trust anchors", () => {
  it("loads all stamped trust material lazily", async () => {
    const module = await load();
    expect(module.getTrustAnchors()).toEqual({
      rootCAPEM: valid.VITE_ROOT_CA_PEM,
      trustDomain: valid.VITE_TRUST_DOMAIN,
      cloudAccountId: valid.VITE_CLOUD_ACCOUNT_ID,
      nodeCRLs,
    });
  });

  it("keeps module import and auth-disabled access usable without trust inputs", async () => {
    const module = await load({
      VITE_AUTH_ENABLED: "",
      VITE_AS_ORIGIN: "",
      VITE_ROOT_CA_PEM: "",
      VITE_TRUST_DOMAIN: "",
      VITE_CLOUD_ACCOUNT_ID: "",
      VITE_NODE_CRL_BUNDLE_JSON: "",
    });
    expect(module.getTrustAnchors()).toEqual({
      rootCAPEM: "",
      trustDomain: "",
      cloudAccountId: "",
      nodeCRLs: [],
    });
  });

  it.each([
    { VITE_AUTH_ENABLED: "true", VITE_AS_ORIGIN: "" },
    { VITE_AUTH_ENABLED: "", VITE_AS_ORIGIN: "https://as.spawnery.dev" },
  ])("fails closed for every application auth-enabled mode", async (authEnv) => {
    const module = await load({
      ...authEnv,
      VITE_ROOT_CA_PEM: "",
      VITE_TRUST_DOMAIN: "",
      VITE_CLOUD_ACCOUNT_ID: "",
      VITE_NODE_CRL_BUNDLE_JSON: "",
    });
    expect(() => module.getTrustAnchors()).toThrow("VITE_ROOT_CA_PEM");
  });

  it("defers auth-enabled validation until the getter is called", async () => {
    const module = await load({ VITE_ROOT_CA_PEM: "" });
    expect(() => module.getTrustAnchors()).toThrow("VITE_ROOT_CA_PEM");
  });

  it.each([
    "VITE_ROOT_CA_PEM",
    "VITE_TRUST_DOMAIN",
    "VITE_CLOUD_ACCOUNT_ID",
    "VITE_NODE_CRL_BUNDLE_JSON",
  ] as const)("fails closed when auth-enabled %s is empty", async (name) => {
    const module = await load({ [name]: "" });
    expect(() => module.getTrustAnchors()).toThrow(name);
  });

  it("does not retain eager pin exports", async () => {
    const module = await load();
    expect(Object.keys(module)).toEqual(["getTrustAnchors"]);
  });
});
