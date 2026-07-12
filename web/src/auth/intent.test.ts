import { beforeEach, describe, expect, it, vi } from "vitest";
import { requireSessionSigningKeys } from "./intent";
import { MemoryKeyStore } from "./keystore";
import { useSessionStore } from "./session";

describe("requireSessionSigningKeys", () => {
  beforeEach(() => {
    useSessionStore.setState({
      status: "authed",
      cpAccessToken: "cp-token",
      nodeAccessToken: "node-token",
      refreshTokenHash: "rth",
      account: { accountId: "acct-1", handle: "alice" },
      keyStore: new MemoryKeyStore(),
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 200 })));
  });

  it("enters key-loss recovery rather than generating when the persistent key is missing", async () => {
    await expect(requireSessionSigningKeys()).rejects.toThrow("persistent session key");
    const state = useSessionStore.getState();
    expect(state.status).toBe("key-lost");
    expect(state.cpAccessToken).toBe("");
    expect(state.nodeAccessToken).toBe("");
  });

  it("enters key-loss recovery when the stored private key cannot sign", async () => {
    const keys = await crypto.subtle.generateKey(
      { name: "ECDSA", namedCurve: "P-256" }, false, ["sign", "verify"],
    ) as CryptoKeyPair;
    const store = new MemoryKeyStore();
    await store.put({ privateKey: keys.publicKey, publicKey: keys.publicKey });
    useSessionStore.setState({ keyStore: store });
    await expect(requireSessionSigningKeys()).rejects.toThrow("persistent session key");
    expect(useSessionStore.getState().status).toBe("key-lost");
  });
});
