import { describe, it, expect } from "vitest";
import { loadTenancyConfig, classifyConnectError, isResourceExhausted } from "./tenancy";

function baseEnv(overrides: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  return {
    ACC_APP_ID: "acc/app",
    ACC_IDENTITY_POOL: "tok1=owner1,tok2=owner2,tok3=owner3",
    ...overrides,
  };
}

describe("loadTenancyConfig", () => {
  it("throws a clear error when ACC_APP_ID is missing", () => {
    const env = baseEnv();
    delete env.ACC_APP_ID;
    expect(() => loadTenancyConfig(env)).toThrow(/ACC_APP_ID/);
  });

  it("parses ACC_APP_ID and optional ACC_MODEL", () => {
    const cfg = loadTenancyConfig(baseEnv({ ACC_MODEL: "gpt-5" }));
    expect(cfg.appId).toBe("acc/app");
    expect(cfg.model).toBe("gpt-5");
  });

  it("leaves model undefined when ACC_MODEL is unset", () => {
    const cfg = loadTenancyConfig(baseEnv());
    expect(cfg.model).toBeUndefined();
  });

  it("parses explicit ACC_TENANCY_A / ACC_TENANCY_B", () => {
    const cfg = loadTenancyConfig(
      baseEnv({ ACC_TENANCY_A: "tokA=ownerA", ACC_TENANCY_B: "tokB=ownerB" }),
    );
    expect(cfg.a).toEqual({ token: "tokA", owner: "ownerA" });
    expect(cfg.b).toEqual({ token: "tokB", owner: "ownerB" });
  });

  it("falls back to the first two pool entries when A/B are unset", () => {
    const cfg = loadTenancyConfig(baseEnv());
    expect(cfg.a).toEqual({ token: "tok1", owner: "owner1" });
    expect(cfg.b).toEqual({ token: "tok2", owner: "owner2" });
  });

  it("throws when the explicit A and B owners are identical", () => {
    const env = baseEnv({ ACC_TENANCY_A: "tokA=same-owner", ACC_TENANCY_B: "tokB=same-owner" });
    expect(() => loadTenancyConfig(env)).toThrow(/owner/i);
  });

  it("throws when falling back to a pool with fewer than 2 entries", () => {
    const env = baseEnv({ ACC_IDENTITY_POOL: "tok1=owner1" });
    expect(() => loadTenancyConfig(env)).toThrow(/pool/i);
  });

  it("leaves quota undefined when ACC_QUOTA_CAP is unset", () => {
    const cfg = loadTenancyConfig(baseEnv({ ACC_QUOTA_TOKEN: "tokQ", ACC_QUOTA_OWNER: "ownerQ" }));
    expect(cfg.quota).toBeUndefined();
  });

  it("leaves quota undefined when ACC_QUOTA_CAP is 0", () => {
    const cfg = loadTenancyConfig(
      baseEnv({ ACC_QUOTA_TOKEN: "tokQ", ACC_QUOTA_OWNER: "ownerQ", ACC_QUOTA_CAP: "0" }),
    );
    expect(cfg.quota).toBeUndefined();
  });

  it("leaves quota undefined when ACC_QUOTA_CAP is non-numeric", () => {
    const cfg = loadTenancyConfig(
      baseEnv({ ACC_QUOTA_TOKEN: "tokQ", ACC_QUOTA_OWNER: "ownerQ", ACC_QUOTA_CAP: "nope" }),
    );
    expect(cfg.quota).toBeUndefined();
  });

  it("leaves quota undefined when ACC_QUOTA_TOKEN is missing even if ACC_QUOTA_CAP is set", () => {
    const cfg = loadTenancyConfig(baseEnv({ ACC_QUOTA_OWNER: "ownerQ", ACC_QUOTA_CAP: "3" }));
    expect(cfg.quota).toBeUndefined();
  });

  it("parses the quota block when cap>0 and token is set", () => {
    const cfg = loadTenancyConfig(
      baseEnv({ ACC_QUOTA_TOKEN: "tokQ", ACC_QUOTA_OWNER: "ownerQ", ACC_QUOTA_CAP: "3" }),
    );
    expect(cfg.quota).toEqual({
      identity: { token: "tokQ", owner: "ownerQ" },
      owner: "ownerQ",
      cap: 3,
    });
  });
});

describe("classifyConnectError", () => {
  it("pulls code/message from a Connect-JSON error body", () => {
    const got = classifyConnectError(429, { code: "resource_exhausted", message: "spawn limit reached (3)" });
    expect(got).toEqual({ code: "resource_exhausted", message: "spawn limit reached (3)" });
  });

  it("returns an empty object for a non-object body", () => {
    expect(classifyConnectError(429, "not json")).toEqual({});
    expect(classifyConnectError(429, null)).toEqual({});
    expect(classifyConnectError(429, undefined)).toEqual({});
  });
});

describe("isResourceExhausted", () => {
  it("is true for status 429 + code resource_exhausted", () => {
    expect(isResourceExhausted({ status: 429, code: "resource_exhausted" })).toBe(true);
  });

  it("is false for status 403", () => {
    expect(isResourceExhausted({ status: 403, code: "resource_exhausted" })).toBe(false);
  });

  it("is false for status 200 with no code", () => {
    expect(isResourceExhausted({ status: 200 })).toBe(false);
  });

  it("is false for status 429 with a different code", () => {
    expect(isResourceExhausted({ status: 429, code: "permission_denied" })).toBe(false);
  });
});
