/**
 * Tests for recovery module: M8 warning verbatim text ([WM12]) and the
 * CAS retry-rebase loop ([WM1]) exercised through the exported
 * buildAndAppendWithRetry helper.
 */

import { describe, it, expect } from "vitest";
import { M8_TRUSTED_DEVICE_WARNING, buildAndAppendWithRetry } from "./recovery";
import { ConflictError, type ASTransport, type StoredEntry, type AppendResult } from "./deviceset";

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

// ── [WM1] Hermetic CAS retry-rebase tests for buildAndAppendWithRetry ────────

/** A minimal dummy StoredEntry for transport-layer tests (no crypto needed). */
const DUMMY_ENTRY: StoredEntry = { body: btoa("{}"), sigs: [] };

describe("[WM1] buildAndAppendWithRetry", () => {
  /**
   * Test 3: append throws ConflictError once, then succeeds.
   * Verifies: result == success AppendResult, rebaseFn called once, buildFn called twice.
   */
  it("retries on conflict and succeeds on second attempt", async () => {
    let buildCallCount = 0;
    let rebaseCallCount = 0;
    let appendCallCount = 0;

    const fake: ASTransport = {
      fetchLog: async () => ({ log: { entries: [] }, head: "", version: 0 }),
      append: async (_entry: StoredEntry): Promise<AppendResult> => {
        appendCallCount++;
        if (appendCallCount === 1) throw new ConflictError("new-head", 2);
        return { version: 2, head: "new-head" };
      },
    };

    const buildFn = async (): Promise<StoredEntry> => {
      buildCallCount++;
      return DUMMY_ENTRY;
    };
    const rebaseFn = async (): Promise<void> => {
      rebaseCallCount++;
    };

    const result = await buildAndAppendWithRetry(fake, buildFn, rebaseFn);

    expect(result).toEqual({ version: 2, head: "new-head" });
    expect(appendCallCount).toBe(2);
    expect(rebaseCallCount).toBe(1);
    expect(buildCallCount).toBe(2);
  });

  /**
   * Test 4: append always conflicts → give-up after maxRetries.
   * Verifies: rejects with ConflictError, rebaseFn called maxRetries times,
   * buildFn called maxRetries+1 times (one initial + one per retry).
   */
  it("gives up with ConflictError after maxRetries failed appends", async () => {
    let buildCallCount = 0;
    let rebaseCallCount = 0;
    let appendCallCount = 0;
    const maxRetries = 5;

    const fake: ASTransport = {
      fetchLog: async () => ({ log: { entries: [] }, head: "", version: 0 }),
      append: async (): Promise<AppendResult> => {
        appendCallCount++;
        throw new ConflictError("head", appendCallCount);
      },
    };

    const buildFn = async (): Promise<StoredEntry> => {
      buildCallCount++;
      return DUMMY_ENTRY;
    };
    const rebaseFn = async (): Promise<void> => {
      rebaseCallCount++;
    };

    await expect(buildAndAppendWithRetry(fake, buildFn, rebaseFn, maxRetries)).rejects.toBeInstanceOf(
      ConflictError,
    );

    // Initial attempt + maxRetries = maxRetries+1 total append calls
    expect(appendCallCount).toBe(maxRetries + 1);
    expect(rebaseCallCount).toBe(maxRetries);
    expect(buildCallCount).toBe(maxRetries + 1);
  });
});
