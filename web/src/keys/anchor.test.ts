/**
 * Tests for local trust-anchor persistence ([WM5][WM6]).
 */

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { saveAnchor, loadAnchor, clearAnchor, bumpPinnedHead } from "./anchor";
import type { DeviceAnchor } from "./anchor";

const sampleAnchor: DeviceAnchor = {
  ownerRoot: {
    device1_sign_pub: "dGVzdC1kZXZpY2Utc2lnbi1wdWI=",
    recovery_sign_pub: "dGVzdC1yZWNvdmVyeS1zaWduLXB1Yg==",
  },
  headVersion: 3,
};

describe("anchor persistence", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it("round-trips through saveAnchor/loadAnchor", () => {
    saveAnchor(sampleAnchor);
    const loaded = loadAnchor();
    expect(loaded).not.toBeNull();
    expect(loaded!.headVersion).toBe(3);
    expect(loaded!.ownerRoot.device1_sign_pub).toBe(sampleAnchor.ownerRoot.device1_sign_pub);
    expect(loaded!.ownerRoot.recovery_sign_pub).toBe(sampleAnchor.ownerRoot.recovery_sign_pub);
  });

  it("returns null when absent", () => {
    expect(loadAnchor()).toBeNull();
  });

  it("clearAnchor removes the stored anchor", () => {
    saveAnchor(sampleAnchor);
    clearAnchor();
    expect(loadAnchor()).toBeNull();
  });
});

describe("bumpPinnedHead [WM6]", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it("advances the pinned head version", () => {
    saveAnchor(sampleAnchor); // headVersion = 3
    bumpPinnedHead(5);
    expect(loadAnchor()!.headVersion).toBe(5);
  });

  it("allows bumping to the same version (no-op advance)", () => {
    saveAnchor(sampleAnchor); // headVersion = 3
    bumpPinnedHead(3);
    expect(loadAnchor()!.headVersion).toBe(3);
  });

  it("rejects regression (new < current)", () => {
    saveAnchor(sampleAnchor); // headVersion = 3
    expect(() => bumpPinnedHead(2)).toThrow(/regression/);
  });

  it("throws when no anchor is persisted", () => {
    expect(() => bumpPinnedHead(1)).toThrow(/no anchor/);
  });

  it("preserves ownerRoot when bumping head", () => {
    saveAnchor(sampleAnchor);
    bumpPinnedHead(10);
    const loaded = loadAnchor()!;
    expect(loaded.ownerRoot.device1_sign_pub).toBe(sampleAnchor.ownerRoot.device1_sign_pub);
    expect(loaded.ownerRoot.recovery_sign_pub).toBe(sampleAnchor.ownerRoot.recovery_sign_pub);
  });
});
