import { describe, it, expect } from "vitest";
import { parseIdentityPool, identityForWorker } from "./identity-pool";

describe("parseIdentityPool", () => {
  it("parses token=owner pairs", () => {
    expect(parseIdentityPool("t1=o1,t2=o2,t3=o3")).toEqual([
      { token: "t1", owner: "o1" },
      { token: "t2", owner: "o2" },
      { token: "t3", owner: "o3" },
    ]);
  });

  it("throws on a malformed entry", () => {
    expect(() => parseIdentityPool("t1-o1")).toThrow(/not "token=owner"/);
  });

  it("ignores blank entries from stray commas", () => {
    expect(parseIdentityPool("t1=o1,,t2=o2,")).toEqual([
      { token: "t1", owner: "o1" },
      { token: "t2", owner: "o2" },
    ]);
  });
});

describe("identityForWorker", () => {
  const pool = parseIdentityPool("t0=o0,t1=o1,t2=o2");

  it("maps worker 0/1/2 to distinct owners", () => {
    expect(identityForWorker(pool, 0)).toEqual({ token: "t0", owner: "o0" });
    expect(identityForWorker(pool, 1)).toEqual({ token: "t1", owner: "o1" });
    expect(identityForWorker(pool, 2)).toEqual({ token: "t2", owner: "o2" });
  });

  it("throws when parallelIndex exceeds the pool size", () => {
    expect(() => identityForWorker(pool, 3)).toThrow(/pool has 3 entries/);
  });
});
