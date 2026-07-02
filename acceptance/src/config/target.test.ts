import { describe, it, expect } from "vitest";
import { isNonProd, loadTargetConfig } from "./target";

describe("isNonProd", () => {
  it("treats localhost as non-prod", () => {
    expect(isNonProd("localhost", [])).toBe(true);
    expect(isNonProd("127.0.0.1", [])).toBe(true);
  });

  it("treats an allowlisted host as non-prod (exact match)", () => {
    expect(isNonProd("blacky.dayton", ["blacky.dayton"])).toBe(true);
  });

  it("treats a subdomain of an allowlisted suffix as non-prod", () => {
    expect(isNonProd("staging.blacky.dayton", ["blacky.dayton"])).toBe(true);
  });

  it("default-denies an unlisted host as prod", () => {
    expect(isNonProd("app.spawnery.com", ["blacky.dayton"])).toBe(false);
  });

  it("default-denies with an empty allowlist", () => {
    expect(isNonProd("app.spawnery.com", [])).toBe(false);
  });

  it("is case-insensitive", () => {
    expect(isNonProd("BLACKY.DAYTON", ["blacky.dayton"])).toBe(true);
  });
});

describe("loadTargetConfig", () => {
  const baseEnv = {
    ACC_WEB_ORIGIN: "https://blacky.dayton:5173",
    ACC_CP_ENDPOINT: "https://blacky.dayton:5173",
    ACC_IDENTITY_POOL: "t1=o1,t2=o2",
    ACC_TARGET_REF: "dev",
    ACC_BUILD_REF: "dev",
  };

  it("throws naming the missing var when a required var is absent", () => {
    const env = { ...baseEnv };
    delete (env as Record<string, string | undefined>).ACC_WEB_ORIGIN;
    expect(() => loadTargetConfig(env as unknown as NodeJS.ProcessEnv)).toThrow(/ACC_WEB_ORIGIN/);
  });

  it("derives targetHost from ACC_WEB_ORIGIN", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.targetHost).toBe("blacky.dayton:5173");
  });

  it("parses the identity pool into N entries", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.identityPool).toHaveLength(2);
  });

  it("defaults authMode to dev-token", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.authMode).toBe("dev-token");
  });

  it("rejects an invalid ACC_AUTH_MODE", () => {
    const env = { ...baseEnv, ACC_AUTH_MODE: "bogus" };
    expect(() => loadTargetConfig(env as unknown as NodeJS.ProcessEnv)).toThrow(/ACC_AUTH_MODE/);
  });
});
