/**
 * Tests for minimal protobuf writer + reader using the Go golden vectors
 * from internal/intent/testdata/intent_vectors.json.
 *
 * These tests verify byte-for-byte compatibility with Go proto.Marshal for IntentBody.
 */

import { describe, it, expect } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { ProtoWriter, readFields, encodeVarint, decodeVarint } from "./protobuf";

const VECTORS_PATH = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../../../internal/intent/testdata/intent_vectors.json",
);

function hexToBytes(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < hex.length; i += 2) {
    out[i / 2] = parseInt(hex.slice(i, i + 2), 16);
  }
  return out;
}

function bytesToHex(bytes: Uint8Array): string {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, "0")).join("");
}

describe("ProtoWriter — IntentBody golden vectors", () => {
  const raw = fs.readFileSync(VECTORS_PATH, "utf8");
  const vectors = JSON.parse(raw) as {
    body_bytes_hex: string;
    body_base64: string;
    spki_der_hex: string;
    spki_hash_hex: string;
  };

  // The vector body_bytes_hex encodes this IntentBody (from vectors_test.go):
  //   jti          = "fixed-jti-for-vectors"   field 1 (string)
  //   issued_at    = 1770000000               field 2 (int64) — unix s ≈ 2026-01
  //   spawn_id     = "sp-vec-001"              field 3 (string)
  //   generation   = 1                         field 4 (uint64)
  //   target_node_id = "node-vec-1"            field 5 (string)
  //   op           = "create-spawn"            field 6 (string)
  //   app_ref      = "app/test@sha256:deadbeef" field 7 (string)
  //   image        = "registry/img@sha256:cafebabe" field 8 (string)
  //   model        = "claude-test"             field 9 (string)
  // (data_ref, session_id, mounts are empty/zero — omitted by proto3)

  it("ProtoWriter output byte-matches intent_vectors.json body_bytes_hex", () => {
    const w = new ProtoWriter();
    w.writeBytes(1, "fixed-jti-for-vectors");
    w.writeVarint(2, 1770000000n);
    w.writeBytes(3, "sp-vec-001");
    w.writeVarint(4, 1n);
    w.writeBytes(5, "node-vec-1");
    w.writeBytes(6, "create-spawn");
    w.writeBytes(7, "app/test@sha256:deadbeef");
    w.writeBytes(8, "registry/img@sha256:cafebabe");
    w.writeBytes(9, "claude-test");
    const got = w.finish();
    expect(bytesToHex(got)).toBe(vectors.body_bytes_hex);
  });

  it("ProtoReader decodes intent body fields correctly", () => {
    const bodyBytes = hexToBytes(vectors.body_bytes_hex);
    const fields = readFields(bodyBytes);

    const byNum = new Map(fields.map((f) => [f.fieldNumber, f]));

    const dec = (f: { bytes?: Uint8Array }) => f.bytes ? new TextDecoder().decode(f.bytes) : "";

    expect(dec(byNum.get(1)!)).toBe("fixed-jti-for-vectors");
    expect(byNum.get(2)!.varint).toBe(1770000000n);
    expect(dec(byNum.get(3)!)).toBe("sp-vec-001");
    expect(byNum.get(4)!.varint).toBe(1n);
    expect(dec(byNum.get(5)!)).toBe("node-vec-1");
    expect(dec(byNum.get(6)!)).toBe("create-spawn");
    expect(dec(byNum.get(7)!)).toBe("app/test@sha256:deadbeef");
    expect(dec(byNum.get(8)!)).toBe("registry/img@sha256:cafebabe");
    expect(dec(byNum.get(9)!)).toBe("claude-test");
  });

  it("ProtoWriter omits zero/empty fields", () => {
    const w = new ProtoWriter();
    w.writeVarint(1, 0n);
    w.writeBytes(2, "");
    w.writeBytes(3, new Uint8Array(0));
    expect(w.finish().length).toBe(0);
  });

  it("varint round-trips", () => {
    for (const n of [0n, 1n, 127n, 128n, 16383n, 16384n, 2147483647n, 100000000000n]) {
      const enc = encodeVarint(n);
      const [dec] = decodeVarint(enc, 0);
      expect(dec).toBe(n);
    }
  });

  it("ProtoWriter sorts by field number regardless of insertion order", () => {
    const w1 = new ProtoWriter();
    w1.writeBytes(3, "c");
    w1.writeBytes(1, "a");
    w1.writeBytes(2, "b");

    const w2 = new ProtoWriter();
    w2.writeBytes(1, "a");
    w2.writeBytes(2, "b");
    w2.writeBytes(3, "c");

    expect(bytesToHex(w1.finish())).toBe(bytesToHex(w2.finish()));
  });
});
