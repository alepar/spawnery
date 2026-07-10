import { describe, it, expect, vi } from "vitest";
import * as execModule from "./exec";
import { newMarker, markerPath, writeMarker, readMarker, assertFreshMarker } from "./marker";
import type { ExecConfig } from "./exec";

describe("newMarker", () => {
  it("starts with `<runId>-<spawnId>-`", () => {
    expect(newMarker("r1", "sp-a")).toMatch(/^r1-sp-a-/);
  });

  it("produces distinct values on successive calls", () => {
    expect(newMarker("r1", "sp-a")).not.toBe(newMarker("r1", "sp-a"));
  });
});

describe("markerPath", () => {
  it("is namespaced by runId under the mounted workspace", () => {
    expect(markerPath("r1")).toBe("/app/data/.acc-marker-r1");
  });
});

describe("assertFreshMarker", () => {
  it("does not throw when the (trimmed) marker matches", () => {
    expect(() => assertFreshMarker("m1\n", "m1")).not.toThrow();
  });

  it("throws naming both the got and expected value on a mismatch", () => {
    expect(() => assertFreshMarker("m0", "m1")).toThrow(/m0/);
    expect(() => assertFreshMarker("m0", "m1")).toThrow(/m1/);
  });

  it("frames a mismatch as stale/lost state, not merely absent", () => {
    expect(() => assertFreshMarker("m0", "m1")).toThrow(/stale|lost/i);
  });
});

describe("writeMarker / readMarker", () => {
  const cfg: ExecConfig = { spawnctlBin: "spawnctl", nodeAddr: "http://n:9092" };

  it("writeMarker execs a shell write of the marker to markerPath(runId)", async () => {
    const spy = vi.spyOn(execModule, "execOrThrow").mockResolvedValue({ stdout: "", stderr: "", code: 0 });
    await writeMarker(cfg, "sp-1", "r1", "the-marker");
    expect(spy).toHaveBeenCalledTimes(1);
    const [gotCfg, gotSpawnId, gotCmd] = spy.mock.calls[0];
    expect(gotCfg).toBe(cfg);
    expect(gotSpawnId).toBe("sp-1");
    expect(gotCmd[0]).toBe("sh");
    expect(gotCmd.join(" ")).toContain(markerPath("r1"));
    expect(gotCmd.join(" ")).toContain("the-marker");
    spy.mockRestore();
  });

  it("readMarker execs `cat` on markerPath(runId) and returns stdout untrimmed", async () => {
    const spy = vi.spyOn(execModule, "execOrThrow").mockResolvedValue({ stdout: "the-marker\n", stderr: "", code: 0 });
    const got = await readMarker(cfg, "sp-1", "r1");
    expect(got).toBe("the-marker\n");
    const [, , gotCmd] = spy.mock.calls[0];
    expect(gotCmd).toEqual(["cat", markerPath("r1")]);
    spy.mockRestore();
  });
});
