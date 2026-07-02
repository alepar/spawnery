import { describe, it, expect } from "vitest";
import { newRunId, runTimestamp, nsName, isAccArtifact, runIdOf } from "./namespace";

describe("newRunId / runTimestamp round-trip", () => {
  it("round-trips an embedded timestamp", () => {
    const t = 1_750_000_000_123;
    const id = newRunId(t);
    expect(runTimestamp(id)).toBe(t);
  });

  it("produces distinct ids across calls", () => {
    const a = newRunId(1000);
    const b = newRunId(1000);
    // Same ms timestamp, but the random suffix should (almost certainly) differ.
    expect(a === b).toBe(false);
  });

  it("returns null for an unparseable id", () => {
    expect(runTimestamp("not-a-run-id")).toBeNull();
  });
});

describe("nsName", () => {
  it("builds the acc-<runId>-w<worker>-<base> shape", () => {
    expect(nsName("r123-abcd", 2, "myapp")).toBe("acc-r123-abcd-w2-myapp");
  });
});

describe("isAccArtifact", () => {
  it("is true for acc- prefixed names", () => {
    expect(isAccArtifact("acc-r123-abcd-w0-foo")).toBe(true);
  });

  it("is false for other names", () => {
    expect(isAccArtifact("some-other-spawn")).toBe(false);
    expect(isAccArtifact("")).toBe(false);
  });
});

describe("runIdOf", () => {
  it("extracts the runId component from a namespaced name", () => {
    expect(runIdOf("acc-r123-abcd-w2-myapp")).toBe("r123-abcd");
  });

  it("returns null for a non-acc name", () => {
    expect(runIdOf("myapp")).toBeNull();
  });
});
