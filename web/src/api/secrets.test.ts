import { beforeEach, describe, expect, it, vi } from "vitest";

import { createSecretValue, getEnvelope, listSecretIdsForSweep, openSecretValue, putEnvelope, updateSecretValue } from "./secrets";
import { generateDeviceKeys, exportDeviceRef } from "@/keys/device";
import { openEnvelope, sealEnvelope, type Envelope } from "@/keys/hpke";

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

function fromProtoBytesBase64<T>(value: string): T {
  const bytes = Uint8Array.from(atob(value), (c) => c.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes)) as T;
}

function fetchBody(f: ReturnType<typeof mockFetchSequence>, callIndex: number): string {
  return (f.mock.calls[callIndex][1] as { body: string }).body;
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

  it("putEnvelope rejects unsafe versions and epochs before PutSecret", async () => {
    const unsafeEnvelope: Envelope = {
      at_rest: { account_id: "alice", secret_id: "s1", version: Number.MAX_SAFE_INTEGER + 1 },
      recipients: [],
      nonce: "bm9uY2U=",
      ct: "Y3Q=",
    };
    const currentSecret = {
      secretId: "s1",
      type: "USER_SECRET_TYPE_GENERIC_KV",
      name: "Secret",
      provider: "",
      targetContainer: "ARTIFACT_TARGET_AGENT",
      envVarName: "SECRET_VALUE",
      destPath: "",
      version: "1",
    };
    const f = mockFetchSequence({ secret: currentSecret }, { secret: currentSecret });
    vi.stubGlobal("fetch", f);

    await expect(putEnvelope("s1", unsafeEnvelope, 1)).rejects.toThrow("version");
    expect(f).toHaveBeenCalledTimes(1);

    const validEnvelope: Envelope = {
      ...unsafeEnvelope,
      at_rest: { ...unsafeEnvelope.at_rest, version: 2 },
    };
    await expect(putEnvelope("s1", validEnvelope, Number.NaN)).rejects.toThrow("devicesetEpoch");
  });

  it("createSecretValue POSTs CreateSecret without plaintext and seals version 1 AAD", async () => {
    const deviceKeys = await generateDeviceKeys();
    const deviceRef = await exportDeviceRef(deviceKeys);
    const plaintext = "ghp_create_plaintext";
    const f = mockFetchSequence({ secret: { secretId: "s-create", version: "1" } });
    vi.stubGlobal("fetch", f);

    await createSecretValue({
      accountId: "acct-1",
      secretId: "s-create",
      type: "USER_SECRET_TYPE_GITHUB_TOKEN",
      name: "GitHub token",
      targetContainer: "ARTIFACT_TARGET_AGENT",
      envVarName: "GITHUB_TOKEN",
      devicesetEpoch: 9,
      plaintext,
      recipientPubkeys: [deviceRef.x25519Pub],
    });

    expect(f).toHaveBeenCalledTimes(1);
    expect(f.mock.calls[0][0]).toBe("/cp.v1.SpawnService/CreateSecret");
    const bodyString = fetchBody(f, 0);
    expect(bodyString).not.toContain(plaintext);
    const body = JSON.parse(bodyString);
    expect(body.secret).toMatchObject({
      secretId: "s-create",
      type: "USER_SECRET_TYPE_GITHUB_TOKEN",
      name: "GitHub token",
      provider: "",
      targetContainer: "ARTIFACT_TARGET_AGENT",
      envVarName: "GITHUB_TOKEN",
      destPath: "",
      devicesetEpoch: "9",
    });

    const envelope = fromProtoBytesBase64<Envelope>(body.secret.envelope);
    expect(envelope.at_rest).toEqual({ account_id: "acct-1", secret_id: "s-create", version: 1 });
    const opened = await openEnvelope(envelope, deviceKeys.x25519Private, deviceRef.x25519Pub);
    expect(new TextDecoder().decode(opened)).toBe(plaintext);
  });

  it("updateSecretValue fetches metadata, CASes current version, bumps AAD version, and omits plaintext", async () => {
    const deviceKeys = await generateDeviceKeys();
    const deviceRef = await exportDeviceRef(deviceKeys);
    const currentSecret = {
      secretId: "s-update",
      type: "USER_SECRET_TYPE_INFERENCE_KEY",
      name: "OpenRouter",
      provider: "openrouter",
      targetContainer: "ARTIFACT_TARGET_SIDECAR",
      envVarName: "OPENROUTER_API_KEY",
      destPath: "",
      version: "7",
      devicesetEpoch: "3",
      envelope: toProtoBytesBase64({ at_rest: { account_id: "acct-1", secret_id: "s-update", version: 7 }, recipients: [], nonce: "", ct: "" }),
    };
    const plaintext = "rotated_plaintext";
    const f = mockFetchSequence(
      { secret: currentSecret },
      { secret: { secretId: "s-update", version: "8" } },
    );
    vi.stubGlobal("fetch", f);

    await updateSecretValue({
      accountId: "acct-1",
      secretId: "s-update",
      devicesetEpoch: 10,
      plaintext,
      recipientPubkeys: [deviceRef.x25519Pub],
    });

    expect(f).toHaveBeenCalledTimes(2);
    expect(f.mock.calls[0][0]).toBe("/cp.v1.SpawnService/GetSecret");
    expect(JSON.parse(fetchBody(f, 0))).toEqual({ secretId: "s-update" });
    expect(f.mock.calls[1][0]).toBe("/cp.v1.SpawnService/PutSecret");

    const putBodyString = fetchBody(f, 1);
    expect(putBodyString).not.toContain(plaintext);
    const putBody = JSON.parse(putBodyString);
    expect(putBody.expectedVersion).toBe("7");
    expect(putBody.secret).toMatchObject({
      secretId: "s-update",
      type: "USER_SECRET_TYPE_INFERENCE_KEY",
      name: "OpenRouter",
      provider: "openrouter",
      targetContainer: "ARTIFACT_TARGET_SIDECAR",
      envVarName: "OPENROUTER_API_KEY",
      destPath: "",
      devicesetEpoch: "10",
    });

    const envelope = fromProtoBytesBase64<Envelope>(putBody.secret.envelope);
    expect(envelope.at_rest).toEqual({ account_id: "acct-1", secret_id: "s-update", version: 8 });
    const opened = await openEnvelope(envelope, deviceKeys.x25519Private, deviceRef.x25519Pub);
    expect(new TextDecoder().decode(opened)).toBe(plaintext);
  });

  it("openSecretValue fetches a CP envelope and opens it with the local non-extractable device key", async () => {
    const deviceKeys = await generateDeviceKeys();
    const deviceRef = await exportDeviceRef(deviceKeys);
    expect(deviceKeys.x25519Private.extractable).toBe(false);
    const envelope = await sealEnvelope(
      new TextEncoder().encode("stored plaintext"),
      [deviceRef.x25519Pub],
      { account_id: "acct-1", secret_id: "s-open", version: 2 },
    );
    const f = mockFetchSequence({ secret: { secretId: "s-open", envelope: toProtoBytesBase64(envelope) } });
    vi.stubGlobal("fetch", f);

    const opened = await openSecretValue("s-open", deviceKeys);

    expect(f).toHaveBeenCalledTimes(1);
    expect(f.mock.calls[0][0]).toBe("/cp.v1.SpawnService/GetSecret");
    expect(JSON.parse(fetchBody(f, 0))).toEqual({ secretId: "s-open" });
    expect(new TextDecoder().decode(opened)).toBe("stored plaintext");
  });

  it("rejects empty recipient sets before calling the CP", async () => {
    const f = mockFetchSequence();
    vi.stubGlobal("fetch", f);

    await expect(createSecretValue({
      accountId: "acct-1",
      secretId: "s-empty",
      type: "USER_SECRET_TYPE_GENERIC_KV",
      name: "Secret",
      targetContainer: "ARTIFACT_TARGET_AGENT",
      envVarName: "SECRET_VALUE",
      devicesetEpoch: 1,
      plaintext: "plaintext",
      recipientPubkeys: [],
    })).rejects.toThrow("recipient");
    await expect(updateSecretValue({
      accountId: "acct-1",
      secretId: "s-empty",
      devicesetEpoch: 1,
      plaintext: "plaintext",
      recipientPubkeys: [],
    })).rejects.toThrow("recipient");
    expect(f).not.toHaveBeenCalled();
  });

  it("rejects unsafe current versions before PutSecret", async () => {
    const deviceKeys = await generateDeviceKeys();
    const deviceRef = await exportDeviceRef(deviceKeys);
    const f = mockFetchSequence({
      secret: {
        secretId: "s-unsafe",
        type: "USER_SECRET_TYPE_GENERIC_KV",
        name: "Secret",
        provider: "",
        targetContainer: "ARTIFACT_TARGET_AGENT",
        envVarName: "SECRET_VALUE",
        destPath: "",
        version: String(Number.MAX_SAFE_INTEGER),
      },
    });
    vi.stubGlobal("fetch", f);

    await expect(updateSecretValue({
      accountId: "acct-1",
      secretId: "s-unsafe",
      devicesetEpoch: 1,
      plaintext: "plaintext",
      recipientPubkeys: [deviceRef.x25519Pub],
    })).rejects.toThrow("version");
    expect(f).toHaveBeenCalledTimes(1);
    expect(f.mock.calls[0][0]).toBe("/cp.v1.SpawnService/GetSecret");
  });
});
