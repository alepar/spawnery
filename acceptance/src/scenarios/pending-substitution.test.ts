import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { cpv1 } from "@spawnery/client";
import { describe, expect, it } from "vitest";

import { rewriteReadyPendingIntentResponse } from "./pending-substitution";

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function envelope(ready = true) {
  return {
    status: 207,
    headers: {
      "content-type": "application/json",
      "x-request-id": "request-1",
    },
    body: encoder.encode(JSON.stringify({
      ready,
      pending: { spawnId: "spawn-1", targetNodeId: "node-1" },
      generation: "7",
      targetNodeId: "node-1",
      targetNodeClass: "cloud",
      targetNodeAccountId: "spawnery-system",
      nodeCertChain: "Y2VydC1jaGFpbg==",
      signedSubkey: "c2lnbmVkLXN1YmtleQ==",
    })),
  };
}

describe("rewriteReadyPendingIntentResponse", () => {
  it.each([
    ["targetNodeId", "node-substituted"],
    ["targetNodeClass", "self-hosted"],
    ["targetNodeAccountId", "account-substituted"],
    ["nodeCertChain", "c3Vic3RpdHV0ZWQtY2hhaW4="],
  ] as const)("mutates only %s after ready=true", (field, value) => {
    const original = envelope();
    const before = JSON.parse(decoder.decode(original.body)) as Record<string, unknown>;

    const rewritten = rewriteReadyPendingIntentResponse(original, { field, value });

    expect(rewritten.status).toBe(original.status);
    expect(rewritten.headers).toEqual(original.headers);
    const after = JSON.parse(decoder.decode(rewritten.body)) as Record<string, unknown>;
    expect(after).toEqual({ ...before, [field]: value });
  });

  it("preserves the exact body while ready=false", () => {
    const original = envelope(false);
    const rewritten = rewriteReadyPendingIntentResponse(original, {
      field: "targetNodeId",
      value: "node-substituted",
    });

    expect(rewritten.status).toBe(original.status);
    expect(rewritten.headers).toEqual(original.headers);
    expect(rewritten.body).toBe(original.body);
  });

  it("rejects malformed Connect JSON instead of forwarding a guessed mutation", () => {
    const original = { ...envelope(), body: encoder.encode("not-json") };
    expect(() => rewriteReadyPendingIntentResponse(original, {
      field: "targetNodeId",
      value: "node-substituted",
    })).toThrow("GetPendingIntentResponse JSON");
  });

  it("decodes and re-encodes a ready Connect protobuf response", () => {
    const original = {
      status: 200,
      headers: { "content-type": "application/proto" },
      body: toBinary(cpv1.GetPendingIntentResponseSchema, create(cpv1.GetPendingIntentResponseSchema, {
        ready: true,
        targetNodeId: "node-1",
        targetNodeClass: "cloud",
        targetNodeAccountId: "spawnery-system",
        nodeCertChain: encoder.encode("cert-chain"),
      })),
    };

    const rewritten = rewriteReadyPendingIntentResponse(original, {
      field: "targetNodeId",
      value: "node-substituted",
    });

    expect(rewritten.status).toBe(original.status);
    expect(rewritten.headers).toBe(original.headers);
    expect(fromBinary(cpv1.GetPendingIntentResponseSchema, rewritten.body)).toMatchObject({
      ready: true,
      targetNodeId: "node-substituted",
      targetNodeClass: "cloud",
      targetNodeAccountId: "spawnery-system",
      nodeCertChain: encoder.encode("cert-chain"),
    });
  });
});
