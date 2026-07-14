import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { afterEach, describe, expect, it, vi } from "vitest";
import { authv1, fromBase64, sessionKeyHash, exportSpkiDer, toBase64Url } from "@spawnery/client";
import { MemoryKeyStore } from "./keystore";
import { useSessionStore } from "./session";
import { buildNodeReauthControl, type VerifiedSessionAuthorization } from "./sessionReauth";

function wireToken(opts: {
  audience: "cp" | "node";
  tokenId: string;
  spkiHash: Uint8Array;
}): string {
  const body = toBinary(authv1.SessionTokenBodySchema, create(authv1.SessionTokenBodySchema, {
    accountId: "acct-1",
    handle: "alice",
    tokenId: opts.tokenId,
    audience: opts.audience,
    expiresAt: 1900000000n,
    sessionKeyHash: opts.spkiHash,
    familyId: "family-1",
  }));
  return toBase64Url(toBinary(authv1.SignedAuthArtifactSchema, create(authv1.SignedAuthArtifactSchema, {
    artifactType: "session-token",
    payload: body,
    signature: new Uint8Array(64),
    signerChain: [new Uint8Array([1])],
    keyId: new Uint8Array(32),
  })));
}

const authorization: VerifiedSessionAuthorization = {
  spawnId: "sp-1",
  generation: 7n,
  targetNodeId: "node-1",
  sessionId: "session-2",
  clientId: "client-1",
  attachmentSequence: 1,
};

afterEach(() => vi.unstubAllGlobals());

describe("buildNodeReauthControl", () => {
  it("emits the protocol control with only the refreshed node token and its signed token ID", async () => {
    const pair = await crypto.subtle.generateKey(
      { name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"],
    ) as CryptoKeyPair;
    const hash = await sessionKeyHash(await exportSpkiDer(pair.publicKey));
    const cpAccessToken = wireToken({ audience: "cp", tokenId: "cp-next", spkiHash: hash });
    const nodeAccessToken = wireToken({ audience: "node", tokenId: "node-next", spkiHash: hash });
    const store = new MemoryKeyStore();
    await store.put({ privateKey: pair.privateKey, publicKey: pair.publicKey });
    useSessionStore.setState({
      status: "authed", cpAccessToken, nodeAccessToken, refreshTokenHash: "rth",
      account: { accountId: "acct-1", handle: "alice" }, keyStore: store,
    });

    const control = await buildNodeReauthControl(authorization, nodeAccessToken);
    expect(control).toEqual({
      type: "nodeReauth",
      nodeAccessToken,
      signedIntent: expect.any(String),
    });
    expect(JSON.stringify(control)).not.toContain(cpAccessToken);
    const signed = fromBinary(authv1.SignedIntentSchema, fromBase64(control.signedIntent));
    const body = fromBinary(authv1.IntentBodySchema, signed.body);
    expect(body.newTokenId).toBe("node-next");
    expect(body.spawnId).toBe("sp-1");
    expect(body.generation).toBe(7n);
    expect(body.targetNodeId).toBe("node-1");
    expect(body.sessionId).toBe("session-2");
  });

  it("rejects a stale node token or stale attachment sequence", async () => {
    useSessionStore.setState({ cpAccessToken: "cp", nodeAccessToken: "current" });
    await expect(buildNodeReauthControl(authorization, "stale")).rejects.toThrow();
    await expect(buildNodeReauthControl({ ...authorization, attachmentSequence: 0 }, "current")).rejects.toThrow();
  });

  it("routes a missing persistent key through key-loss recovery", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 200 })));
    useSessionStore.setState({
      status: "authed",
      cpAccessToken: "cp",
      nodeAccessToken: "node",
      keyStore: new MemoryKeyStore(),
    });
    await expect(buildNodeReauthControl(authorization, "node")).rejects.toThrow();
    expect(useSessionStore.getState().status).toBe("key-lost");
  });
});
