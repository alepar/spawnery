/**
 * Tests for SAS (Short Authentication String) derivation ([WM4]).
 *
 * Covers:
 *   - Output format (3 groups of 4 base-36 characters)
 *   - Stability (same inputs → same output)
 *   - [WM4] Named negative: substituted-pubkey MITM attack MUST produce a
 *     different SAS so the human comparison detects the attack.
 *   - Cross-language known vector: deriveSAS matches the Go DeriveSAS output
 *     for fixed ASCII inputs (see internal/secrets/seal/sas_test.go TestDeriveSASKnownVector).
 */

import { describe, it, expect } from "vitest";
import { deriveSAS } from "./sas";

/** SAS format: exactly "xxxx-xxxx-xxxx" where x is a base-36 digit. */
const SAS_FORMAT = /^[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$/;

// Fixed byte arrays for hermetic unit tests (no real key material needed).
const genesis = new TextEncoder().encode("genesis-hash-test");
const head = new TextEncoder().encode("head-hash-test");
const legitX25519 = new TextEncoder().encode("legit-x25519-pub-32bytes-paddxxx");
const legitSign = new TextEncoder().encode("legit-sign-pub-65bytes-padded-xx");

describe("deriveSAS — format", () => {
  it("returns a 12-character base-36 code in xxxx-xxxx-xxxx format", async () => {
    const sas = await deriveSAS(genesis, head, legitX25519, legitSign);
    expect(sas).toMatch(SAS_FORMAT);
    expect(sas.length).toBe(12 + 2); // 12 chars + 2 hyphens
  });
});

describe("deriveSAS — stability", () => {
  it("produces the same code for identical inputs", async () => {
    const a = await deriveSAS(genesis, head, legitX25519, legitSign);
    const b = await deriveSAS(genesis, head, legitX25519, legitSign);
    expect(a).toBe(b);
  });

  it("produces different codes for different genesis hashes", async () => {
    const a = await deriveSAS(genesis, head, legitX25519, legitSign);
    const b = await deriveSAS(
      new TextEncoder().encode("different-genesis"),
      head,
      legitX25519,
      legitSign,
    );
    expect(a).not.toBe(b);
  });
});

// ── [WM4] Named negative: substituted-pubkey MITM must fail the SAS ──────────

describe("deriveSAS — [WM4] MITM substituted-pubkey failure", () => {
  /**
   * Spec §4 named negative: if an active attacker substitutes the new device's
   * x25519 public key in transit, the approver's independently-derived SAS
   * MUST differ from the enrollee's — so the human comparison catches the
   * attack.  If the SAS were the same for a different pubkey, a MITM could
   * enroll their own key undetected.
   */
  it("[WM4] substituted x25519 pubkey produces a different SAS", async () => {
    const mitmX25519 = new TextEncoder().encode("mitm--x25519-pub-32bytes-paddxxx");

    const legitimate = await deriveSAS(genesis, head, legitX25519, legitSign);
    const mitm = await deriveSAS(genesis, head, mitmX25519, legitSign);

    expect(mitm).not.toBe(legitimate);
  });

  /**
   * Same test for the signing pubkey — both pubkeys appear in the SAS preimage
   * so substituting either one must produce a different code.
   */
  it("[WM4] substituted sign pubkey produces a different SAS", async () => {
    const mitmSign = new TextEncoder().encode("mitm--sign-pub-65bytes-padded-xx");

    const legitimate = await deriveSAS(genesis, head, legitX25519, legitSign);
    const mitm = await deriveSAS(genesis, head, legitX25519, mitmSign);

    expect(mitm).not.toBe(legitimate);
  });
});

// ── Cross-language known vector ───────────────────────────────────────────────

describe("deriveSAS — cross-language known vector", () => {
  /**
   * Fixed ASCII inputs shared with the Go test (TestDeriveSASKnownVector in
   * internal/secrets/seal/sas_test.go).  If this test fails, the TS and Go
   * deriveSAS implementations have diverged.
   */
  it("matches the Go DeriveSAS output for fixed ASCII inputs", async () => {
    const enc = new TextEncoder();
    const sas = await deriveSAS(
      enc.encode("test-genesis-hash"),
      enc.encode("test-head-hash"),
      enc.encode("test-x25519-pub"),
      enc.encode("test-sign-pub"),
    );
    // Hardcoded value verified against Go DeriveSAS in TestDeriveSASKnownVector.
    expect(sas).toBe("0004-53td-gr6k");
  });
});
