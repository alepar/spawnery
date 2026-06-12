/**
 * Tests for enrollment SAS derivation (Phase 5, [WM4][WM5]).
 *
 * The full approveEnrollment / finalizeEnrollment flows require a live AS
 * (network).  These tests cover the pure-crypto SAS path that runs on both
 * the enrollee and the approver side.
 */

import { describe, it, expect } from "vitest";
import {
  computeEnrollmentSAS,
  generateEnrollmentPayload,
  serializeEnrollmentPayload,
  deserializeEnrollmentPayload,
  type EnrollmentPayload,
} from "./enrollment";

const SAS_FORMAT = /^[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$/;

// Fixed chain anchors for hermetic tests (no real chain needed).
const genesisHash = new TextEncoder().encode("genesis-hash-32bytes-padded-xxxx");
const headHash = new TextEncoder().encode("head-hash-32bytes-padded-xxxxxxx");

describe("computeEnrollmentSAS", () => {
  it("returns a correctly formatted SAS code", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const sas = await computeEnrollmentSAS(genesisHash, headHash, payload);
    expect(sas).toMatch(SAS_FORMAT);
  });

  it("produces the same code for the same inputs (enrollee + approver parity)", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const enrolleeSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    const approverSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    expect(enrolleeSAS).toBe(approverSAS);
  });

  /**
   * [WM4] If the enrollee and approver use the same payload bytes but different
   * chain snapshots (e.g. the chain advanced between the link being created and
   * approved), the SAS codes differ — the human notices and must wait for the
   * enrollee to regenerate a fresh link.
   */
  it("[WM4] different head hashes produce different SAS codes", async () => {
    const { payload } = await generateEnrollmentPayload("my-browser");
    const staleSAS = await computeEnrollmentSAS(genesisHash, headHash, payload);
    const freshHead = new TextEncoder().encode("fresh-head-hash-32bytes-padded-x");
    const freshSAS = await computeEnrollmentSAS(genesisHash, freshHead, payload);
    expect(staleSAS).not.toBe(freshSAS);
  });

  /**
   * [WM4] A substituted pubkey in the payload produces a different SAS so a
   * MITM who replaces the new device's pubkeys is detected by the human.
   */
  it("[WM4] substituted payload pubkey produces a different SAS (MITM detection)", async () => {
    const { payload: legitPayload } = await generateEnrollmentPayload("my-browser");
    const { payload: mitmPayload } = await generateEnrollmentPayload("attacker-device");

    // Swap only the x25519 pubkey; keep everything else the same.
    const tamperedPayload: EnrollmentPayload = {
      ...legitPayload,
      x25519Pub: mitmPayload.x25519Pub,
    };

    const legitSAS = await computeEnrollmentSAS(genesisHash, headHash, legitPayload);
    const mitmSAS = await computeEnrollmentSAS(genesisHash, headHash, tamperedPayload);

    expect(mitmSAS).not.toBe(legitSAS);
  });
});

describe("enrollment payload serialization", () => {
  it("round-trips through serialize / deserialize", async () => {
    const { payload } = await generateEnrollmentPayload("round-trip-device");
    const encoded = serializeEnrollmentPayload(payload);
    const decoded = deserializeEnrollmentPayload(encoded);
    expect(decoded.x25519Pub).toBe(payload.x25519Pub);
    expect(decoded.signPub).toBe(payload.signPub);
    expect(decoded.deviceName).toBe(payload.deviceName);
    expect(decoded.expiresAt).toBe(payload.expiresAt);
  });
});
