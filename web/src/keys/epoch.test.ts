/**
 * Re-seal epoch tracking tests [WM2].
 */

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import {
  initSweep,
  markSecretsCompleted,
  markSecretFailed,
  isSweepComplete,
  remainingCount,
  loadSweepProgress,
  clearSweepProgress,
} from "./epoch";

// jsdom provides localStorage
describe("re-seal epoch sweep [WM2]", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    localStorage.clear();
  });

  it("initSweep creates a sweep with correct initial state", () => {
    const progress = initSweep({
      targetVersion: 3,
      secretIds: ["s1", "s2", "s3"],
      isRevocation: true,
      revokedX25519Pub: "abc",
    });
    expect(progress.targetVersion).toBe(3);
    expect(progress.total).toBe(3);
    expect(progress.done).toBe(0);
    expect(progress.completed).toHaveLength(0);
    expect(progress.failed).toHaveLength(0);
    expect(progress.isRevocation).toBe(true);
    expect(progress.revokedX25519Pub).toBe("abc");
  });

  it("isSweepComplete returns false for partial sweep", () => {
    const p = initSweep({ targetVersion: 1, secretIds: ["a", "b"], isRevocation: false });
    expect(isSweepComplete(p)).toBe(false);
  });

  it("isSweepComplete returns true when all done", () => {
    let p = initSweep({ targetVersion: 1, secretIds: ["a"], isRevocation: false });
    p = markSecretsCompleted(p, ["a"]);
    expect(isSweepComplete(p)).toBe(true);
  });

  it("remainingCount decreases as secrets are completed", () => {
    let p = initSweep({ targetVersion: 1, secretIds: ["a", "b", "c"], isRevocation: true });
    expect(remainingCount(p)).toBe(3);
    p = markSecretsCompleted(p, ["a", "b"]);
    expect(remainingCount(p)).toBe(1);
    p = markSecretsCompleted(p, ["c"]);
    expect(remainingCount(p)).toBe(0);
  });

  it("markSecretFailed does not advance done count", () => {
    let p = initSweep({ targetVersion: 1, secretIds: ["a", "b"], isRevocation: false });
    p = markSecretFailed(p, "a");
    expect(p.done).toBe(0);
    expect(p.failed).toContain("a");
    expect(isSweepComplete(p)).toBe(false);
  });

  it("progress persists to and loads from localStorage", () => {
    initSweep({ targetVersion: 2, secretIds: ["x"], isRevocation: true });
    const loaded = loadSweepProgress();
    expect(loaded).not.toBeNull();
    expect(loaded!.targetVersion).toBe(2);
    expect(loaded!.total).toBe(1);
  });

  it("clearSweepProgress removes persisted state", () => {
    initSweep({ targetVersion: 1, secretIds: [], isRevocation: false });
    clearSweepProgress();
    expect(loadSweepProgress()).toBeNull();
  });

  it("loadSweepProgress returns null when nothing is stored", () => {
    expect(loadSweepProgress()).toBeNull();
  });
});
