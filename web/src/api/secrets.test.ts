import { beforeEach, describe, expect, it, vi } from "vitest";

import { getEnvelope, listSecretIdsForSweep, putEnvelope } from "./secrets";
import type { Envelope } from "@/keys/hpke";

function mockFetchSequence(...jsonBodies: unknown[]) {
  return vi.fn().mockImplementation((_url: string, _init: unknown) => {
    const body = jsonBodies.shift();
    if (body === undefined) {
      throw new Error("unexpected fetch call");
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      json: async () => body,
      text: async () => JSON.stringify(body),
    });
  });
}

function toProtoBytesBase64(value: unknown): string {
  let bin = "";
  for (const byte of new TextEncoder().encode(JSON.stringify(value))) {
    bin += String.fromCharCode(byte);
  }
  return btoa(bin);
}

describe("secrets api", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("listSecretIdsForSweep POSTs ListSecrets with devicesetEpochBefore and maps ids", async () => {
    const f = mockFetchSequence({ secrets: [{ secretId: "s1" }, { secretId: "s2" }] });
    vi.stubGlobal("fetch", f);

    await expect(listSecretIdsForSweep(7)).resolves.toEqual(["s1", "s2"]);
    expect(f).toHaveBeenCalledWith("/cp.v1.SpawnService/ListSecrets", expect.objectContaining({ method: "POST" }));
    expect(JSON.parse((f.mock.calls[0][1] as { body: string }).body)).toEqual({ devicesetEpochBefore: "7" });
  });

  it("getEnvelope decodes proto bytes envelope JSON", async () => {
    const envelope = {
      at_rest: { account_id: "alice", secret_id: "s1", version: 3 },
      recipients: [],
      nonce: "bm9uY2U=",
      ct: "Y3Q=",
    };
    const f = mockFetchSequence({ secret: { secretId: "s1", envelope: toProtoBytesBase64(envelope) } });
    vi.stubGlobal("fetch", f);

    await expect(getEnvelope("s1")).resolves.toEqual(envelope);
    expect(JSON.parse((f.mock.calls[0][1] as { body: string }).body)).toEqual({ secretId: "s1" });
  });

  it("putEnvelope fetches current metadata, then POSTs PutSecret with CAS and deviceset epoch", async () => {
    const currentSecret = {
      secretId: "s1",
      type: "USER_SECRET_TYPE_INFERENCE_KEY",
      name: "OpenRouter",
      provider: "openrouter",
      targetContainer: "ARTIFACT_TARGET_SIDECAR",
      envVarName: "OPENROUTER_API_KEY",
      destPath: "",
      version: "1",
      devicesetEpoch: "1",
      envelope: toProtoBytesBase64({ at_rest: { account_id: "alice", secret_id: "s1", version: 1 }, recipients: [], nonce: "", ct: "" }),
    };
    const f = mockFetchSequence(
      { secret: currentSecret },
      { secret: { secretId: "s1", version: "2" } },
    );
    vi.stubGlobal("fetch", f);

    const nextEnvelope: Envelope = {
      at_rest: { account_id: "alice", secret_id: "s1", version: 2 },
      recipients: [],
      nonce: "bm9uY2U=",
      ct: "Y3Q=",
    };
    await putEnvelope("s1", nextEnvelope, 7);

    expect(f).toHaveBeenCalledTimes(2);
    expect(f.mock.calls[0][0]).toBe("/cp.v1.SpawnService/GetSecret");
    expect(f.mock.calls[1][0]).toBe("/cp.v1.SpawnService/PutSecret");
    expect(JSON.parse((f.mock.calls[1][1] as { body: string }).body)).toEqual({
      expectedVersion: "1",
      secret: {
        secretId: "s1",
        type: "USER_SECRET_TYPE_INFERENCE_KEY",
        name: "OpenRouter",
        provider: "openrouter",
        targetContainer: "ARTIFACT_TARGET_SIDECAR",
        envVarName: "OPENROUTER_API_KEY",
        destPath: "",
        devicesetEpoch: "7",
        envelope: toProtoBytesBase64(nextEnvelope),
      },
    });
  });
});
