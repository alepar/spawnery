/**
 * Tests for recovery module constants and M8 warning verbatim text ([WM12]).
 *
 * Full recoverAndRotate flow requires a live AS; these tests cover the
 * pure-constant and unit-testable surface.
 */

import { describe, it, expect } from "vitest";
import { M8_TRUSTED_DEVICE_WARNING } from "./recovery";

// ── [WM12] M8 warning verbatim from spec §3 ──────────────────────────────────

describe("M8_TRUSTED_DEVICE_WARNING", () => {
  /**
   * [WM12] The constant must reproduce the verbatim banner copy from the
   * owner-sealed spec §3: "approve from your phone / enter recovery code only
   * on a trusted device".  Any paraphrase risks understating the risk or
   * failing consistency checks with the CLI side.
   */
  it("[WM12] contains the verbatim spec §3 banner copy", () => {
    const specText =
      "approve from your phone / enter recovery code only on a trusted device";
    expect(M8_TRUSTED_DEVICE_WARNING).toContain(specText);
  });

  it("is non-empty", () => {
    expect(M8_TRUSTED_DEVICE_WARNING.length).toBeGreaterThan(0);
  });
});
