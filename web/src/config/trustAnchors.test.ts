import { afterEach, describe, expect, it, vi } from "vitest";

const valid = {
  VITE_ROOT_CA_PEM: "-----BEGIN CERTIFICATE-----\nreal-root\n-----END CERTIFICATE-----",
  VITE_TRUST_DOMAIN: "prod.spawnery.internal",
  VITE_CLOUD_ACCOUNT_ID: "spawnery-system",
};

async function load(overrides: Partial<typeof valid> = {}) {
  vi.resetModules();
  for (const [name, value] of Object.entries({ ...valid, ...overrides })) vi.stubEnv(name, value);
  return import("./trustAnchors");
}

afterEach(() => vi.unstubAllEnvs());

describe("release trust anchors", () => {
  it("loads all three pins from required Vite build inputs", async () => {
    const pins = await load();
    expect(pins.PINNED_ROOT_CA_PEM).toBe(valid.VITE_ROOT_CA_PEM);
    expect(pins.PINNED_TRUST_DOMAIN).toBe(valid.VITE_TRUST_DOMAIN);
    expect(pins.PINNED_CLOUD_ACCOUNT_ID).toBe(valid.VITE_CLOUD_ACCOUNT_ID);
  });

  it.each([
    "VITE_ROOT_CA_PEM",
    "VITE_TRUST_DOMAIN",
    "VITE_CLOUD_ACCOUNT_ID",
  ] as const)("fails closed when %s is empty", async (name) => {
    await expect(load({ [name]: "" })).rejects.toThrow(name);
  });

  it.each([
    "VITE_ROOT_CA_PEM",
    "VITE_TRUST_DOMAIN",
    "VITE_CLOUD_ACCOUNT_ID",
  ] as const)("fails closed when %s contains a placeholder", async (name) => {
    await expect(load({ [name]: "not-configured-placeholder" })).rejects.toThrow(name);
  });
});
