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
  const oauthEnv = {
    ...baseEnv,
    ACC_AUTH_MODE: "oauth-pop",
    ACC_ROOT_CA_PEM: "/run/root.pem",
    ACC_TRUST_DOMAIN: "prod.spawnery.internal",
    ACC_CLOUD_ACCOUNT_ID: "spawnery-system",
    ACC_CRL_STATE: "/run/crl-state.json",
    ACC_CRL_ISSUERS: "/run/cloud-intermediate.pem",
    ACC_CRLS: "/run/cloud.crl.pem",
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

  it("accepts oauth-pop as a valid ACC_AUTH_MODE", () => {
    expect(loadTargetConfig(oauthEnv as unknown as NodeJS.ProcessEnv).authMode).toBe("oauth-pop");
  });

  it("defaults fake GitHub link bootstrap off and permits only an explicit oauth-pop 1", () => {
    expect(loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv).bootstrapFakeGitHubLinks).toBe(false);
    expect(loadTargetConfig({
      ...oauthEnv,
      ACC_BOOTSTRAP_FAKE_GITHUB_LINKS: "1",
    } as unknown as NodeJS.ProcessEnv).bootstrapFakeGitHubLinks).toBe(true);
    expect(() => loadTargetConfig({
      ...baseEnv,
      ACC_BOOTSTRAP_FAKE_GITHUB_LINKS: "1",
    } as unknown as NodeJS.ProcessEnv)).toThrow(/oauth-pop/);
    expect(() => loadTargetConfig({
      ...oauthEnv,
      ACC_BOOTSTRAP_FAKE_GITHUB_LINKS: "yes",
    } as unknown as NodeJS.ProcessEnv)).toThrow(/ACC_BOOTSTRAP_FAKE_GITHUB_LINKS/);
  });

  it("requires every public trust input in oauth-pop mode", () => {
    const env = { ...baseEnv, ACC_AUTH_MODE: "oauth-pop" };
    expect(() => loadTargetConfig(env as unknown as NodeJS.ProcessEnv)).toThrow(/ACC_ROOT_CA_PEM/);
  });

  it("defaults asOrigin to webOrigin (same-origin, mirrors the SPA's dev-proxy default)", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.asOrigin).toBe(cfg.webOrigin);
  });

  it("honors an explicit ACC_AS_ORIGIN override", () => {
    const env = { ...baseEnv, ACC_AS_ORIGIN: "https://as.blacky.dayton:8090" };
    const cfg = loadTargetConfig(env as unknown as NodeJS.ProcessEnv);
    expect(cfg.asOrigin).toBe("https://as.blacky.dayton:8090");
  });

  it("leaves seedSkillAppId undefined when ACC_SEED_SKILL_APP_ID is unset (no safe default)", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.seedSkillAppId).toBeUndefined();
  });

  it("sets seedSkillAppId from ACC_SEED_SKILL_APP_ID", () => {
    const env = { ...baseEnv, ACC_SEED_SKILL_APP_ID: "spawnery/secret-app" };
    const cfg = loadTargetConfig(env as unknown as NodeJS.ProcessEnv);
    expect(cfg.seedSkillAppId).toBe("spawnery/secret-app");
  });

  it("leaves agentModel/agentAppId undefined when unset, and does not throw (agent vars are optional)", () => {
    const cfg = loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv);
    expect(cfg.agentModel).toBeUndefined();
    expect(cfg.agentAppId).toBeUndefined();
  });

  it("parses agentModel/agentAppId when present", () => {
    const env = { ...baseEnv, ACC_AGENT_MODEL: "cheap/model", ACC_AGENT_APP_ID: "acc/agent-app" };
    const cfg = loadTargetConfig(env as unknown as NodeJS.ProcessEnv);
    expect(cfg.agentModel).toBe("cheap/model");
    expect(cfg.agentAppId).toBe("acc/agent-app");
  });

  it("parses an explicit @agent inference capability", () => {
    expect(loadTargetConfig({
      ...baseEnv,
      ACC_AGENT_INFERENCE_AVAILABLE: "1",
    } as unknown as NodeJS.ProcessEnv).agentInferenceAvailable).toBe(true);
    expect(loadTargetConfig({
      ...baseEnv,
      ACC_AGENT_INFERENCE_AVAILABLE: "0",
    } as unknown as NodeJS.ProcessEnv).agentInferenceAvailable).toBe(false);
  });

  it("leaves @agent inference capability undefined when the target did not declare it", () => {
    expect(loadTargetConfig(baseEnv as unknown as NodeJS.ProcessEnv).agentInferenceAvailable).toBeUndefined();
  });

  it("rejects an invalid @agent inference capability", () => {
    expect(() => loadTargetConfig({
      ...baseEnv,
      ACC_AGENT_INFERENCE_AVAILABLE: "maybe",
    } as unknown as NodeJS.ProcessEnv)).toThrow(/ACC_AGENT_INFERENCE_AVAILABLE/);
  });
});
