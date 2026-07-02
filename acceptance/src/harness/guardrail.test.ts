import { describe, it, expect } from "vitest";
import { guardMutation, ProdGuardrailError } from "./guardrail";

describe("guardMutation — default-deny matrix", () => {
  it("throws for an untagged test against prod (untagged == mutating)", () => {
    expect(() => guardMutation([], "app.spawnery.com", ["blacky.dayton"])).toThrow(ProdGuardrailError);
  });

  it("throws for an explicitly @mutating test against prod", () => {
    expect(() => guardMutation(["@mutating"], "app.spawnery.com", ["blacky.dayton"])).toThrow(ProdGuardrailError);
  });

  it("allows an untagged (mutating) test against a non-prod host", () => {
    expect(() => guardMutation([], "blacky.dayton", ["blacky.dayton"])).not.toThrow();
  });

  it("allows an @readonly test against prod", () => {
    expect(() => guardMutation(["@readonly"], "app.spawnery.com", ["blacky.dayton"])).not.toThrow();
  });

  it("allows an @mutating test against a non-prod host", () => {
    expect(() => guardMutation(["@mutating"], "blacky.dayton", ["blacky.dayton"])).not.toThrow();
  });

  it("allows an @readonly test against localhost even with an empty allowlist", () => {
    expect(() => guardMutation(["@readonly"], "localhost", [])).not.toThrow();
  });

  it("error message names the blocked host", () => {
    try {
      guardMutation([], "app.spawnery.com", []);
      expect.unreachable();
    } catch (e) {
      expect(e).toBeInstanceOf(ProdGuardrailError);
      expect((e as Error).message).toContain("app.spawnery.com");
    }
  });
});
