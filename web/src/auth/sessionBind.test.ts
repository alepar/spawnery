import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  unary: vi.fn(),
  sign: vi.fn(async (..._args: unknown[]) => "signed-intent-b64"),
  verifyTarget: vi.fn(async (..._args: unknown[]) => undefined),
  requireKeys: vi.fn(async () => ({ privateKey: {}, publicKey: {} })),
}));

vi.mock("@/api/connect", () => ({ unary: mocks.unary }));
vi.mock("@/auth/intent", () => ({
  buildSessionOpenSignedIntentB64: (
    spawnId: string, sessionId: string, generation: bigint, targetNodeId: string, signer: unknown,
  ) => mocks.sign(spawnId, sessionId, generation, targetNodeId, signer),
  requireSessionSigningKeys: () => mocks.requireKeys(),
}));
vi.mock("@/auth/target", () => ({
  verifyResolvedTarget: (target: unknown, accountId: string) => mocks.verifyTarget(target, accountId),
}));
vi.mock("@spawnery/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@spawnery/client")>();
  return { ...actual, WebCryptoSessionSigner: class { constructor(..._args: unknown[]) {} } };
});

let authEnabledValue = false;
vi.mock("@/auth/session", () => ({
  authEnabled: () => authEnabledValue,
  getAccessToken: () => "cp-token",
  getNodeAccessToken: () => "node-token",
  useSessionStore: { getState: () => ({ account: { accountId: "acct-1" } }) },
}));

import { buildSessionBindFrame } from "./sessionBind";

describe("buildSessionBindFrame", () => {
  beforeEach(() => {
    authEnabledValue = false;
    vi.clearAllMocks();
    mocks.unary.mockResolvedValue({
      nodeCertChain: btoa("chain-pem"),
      generation: "3",
      targetNodeId: "node-1",
      targetNodeClass: "self-hosted",
      targetNodeAccountId: "acct-1",
    });
  });

  it("keeps the unauthenticated dev bind path free of signed authorization", async () => {
    const frame = await buildSessionBindFrame("sp1", "0", "client-1", 0);
    expect(frame).toEqual({
      spawnId: "sp1", sessionId: "0", clientId: "client-1",
      token: "cp-token", nodeAccessToken: "node-token", cursor: 0,
    });
    expect(mocks.unary).not.toHaveBeenCalled();
  });

  it("separates CP and node audiences and signs the verified target and generation", async () => {
    authEnabledValue = true;
    const frame = await buildSessionBindFrame("sp1", "2", "client-1", 5);
    expect(frame).toMatchObject({
      token: "cp-token",
      nodeAccessToken: "node-token",
      signedIntent: "signed-intent-b64",
    });
    expect(mocks.unary).toHaveBeenCalledWith("GetSpawnNodeKey", { spawnId: "sp1" });
    expect(mocks.verifyTarget).toHaveBeenCalledWith(expect.objectContaining({
      targetNodeId: "node-1",
      targetNodeClass: "self-hosted",
      targetNodeAccountId: "acct-1",
    }), "acct-1");
    expect(mocks.sign).toHaveBeenCalledWith("sp1", "2", 3n, "node-1", expect.anything());
  });

  it.each([
    ["missing generation", { generation: "0" }],
    ["missing chain", { nodeCertChain: "" }],
    ["missing node ID", { targetNodeId: "" }],
    ["missing class", { targetNodeClass: "" }],
    ["missing account", { targetNodeAccountId: "" }],
  ])("rejects %s instead of sending a fallback bind", async (_name, mutation) => {
    authEnabledValue = true;
    mocks.unary.mockResolvedValue({
      nodeCertChain: btoa("chain-pem"), generation: "3", targetNodeId: "node-1",
      targetNodeClass: "self-hosted", targetNodeAccountId: "acct-1", ...mutation,
    });
    await expect(buildSessionBindFrame("sp1", "0", "client-1", 0)).rejects.toThrow();
    expect(mocks.sign).not.toHaveBeenCalled();
  });

  it("rejects target verification failure before signing", async () => {
    authEnabledValue = true;
    mocks.verifyTarget.mockRejectedValueOnce(new Error("substituted target"));
    await expect(buildSessionBindFrame("sp1", "0", "client-1", 0)).rejects.toThrow("substituted target");
    expect(mocks.sign).not.toHaveBeenCalled();
  });

  it("rejects missing keys and signing failures", async () => {
    authEnabledValue = true;
    mocks.requireKeys.mockRejectedValueOnce(new Error("key lost"));
    await expect(buildSessionBindFrame("sp1", "0", "client-1", 0)).rejects.toThrow("key lost");
    mocks.sign.mockRejectedValueOnce(new Error("sign failed"));
    await expect(buildSessionBindFrame("sp1", "0", "client-1", 0)).rejects.toThrow("sign failed");
  });
});
