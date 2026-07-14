import { describe, it, expect } from "vitest";
import { newMarker, parseUsageTokens, agentAppId, agentInferenceAvailable, agentModel } from "./support";
import type { TargetConfig } from "../../src/config/target";

describe("newMarker", () => {
  it("embeds the runId and a slugified test name", () => {
    const m = newMarker("r1abc-xyz9", "prompt transcript");
    expect(m).toMatch(/^ACC-r1abc-xyz9-prompt-transcript-/);
  });

  it("slugifies non-alphanumeric characters in the test name", () => {
    const m = newMarker("r1", "some @agent flow: reload + exec");
    expect(m).toMatch(/^ACC-r1-some-agent-flow-reload-exec-/);
  });

  it("two calls in the same run+test are unique (freshness — never collide, even in the same ms)", () => {
    const now = () => 1000;
    const a = newMarker("r1", "t", now);
    const b = newMarker("r1", "t", now);
    expect(a).not.toBe(b);
  });

  it("calls across different run ids never collide", () => {
    const a = newMarker("r1", "t");
    const b = newMarker("r2", "t");
    expect(a).not.toBe(b);
  });
});

describe("parseUsageTokens", () => {
  it("parses a 'k'-suffixed token count with a cost segment", () => {
    expect(parseUsageTokens("12.3k tokens · $0.04")).toBe(12300);
  });

  it("parses a plain token count with no cost segment", () => {
    expect(parseUsageTokens("500 tokens")).toBe(500);
  });

  it("returns null for a null badge (agent reported no usage)", () => {
    expect(parseUsageTokens(null)).toBeNull();
  });

  it("returns null for text that doesn't match the expected shape", () => {
    expect(parseUsageTokens("stopped: max tokens exceeded budget")).toBeNull();
  });
});

describe("agentAppId / agentModel", () => {
  const base: TargetConfig = {
    webOrigin: "https://blacky.dayton",
    cpEndpoint: "https://blacky.dayton",
    asOrigin: "https://blacky.dayton",
    env: "dev",
    targetHost: "blacky.dayton",
    authMode: "dev-token",
    bootstrapFakeGitHubLinks: false,
    identityPool: [],
    nonprodHosts: [],
    targetRef: "dev",
    buildRef: "dev",
    spawnctlBin: "spawnctl",
    tokenBudget: 200000,
    wallclockMs: 1800000,
    staleTtlMs: 3600000,
  };

  it("throws naming ACC_AGENT_APP_ID when absent", () => {
    expect(() => agentAppId(base)).toThrow(/ACC_AGENT_APP_ID/);
  });

  it("throws naming ACC_AGENT_MODEL when absent", () => {
    expect(() => agentModel(base)).toThrow(/ACC_AGENT_MODEL/);
  });

  it("returns the configured value when present", () => {
    const cfg = { ...base, agentAppId: "acc/agent-app", agentModel: "cheap/model" };
    expect(agentAppId(cfg)).toBe("acc/agent-app");
    expect(agentModel(cfg)).toBe("cheap/model");
  });

  it("requires targets to declare whether real inference is available", () => {
    expect(() => agentInferenceAvailable(base)).toThrow(/ACC_AGENT_INFERENCE_AVAILABLE/);
  });

  it("returns the target's declared inference capability", () => {
    expect(agentInferenceAvailable({ ...base, agentInferenceAvailable: true })).toBe(true);
    expect(agentInferenceAvailable({ ...base, agentInferenceAvailable: false })).toBe(false);
  });
});
