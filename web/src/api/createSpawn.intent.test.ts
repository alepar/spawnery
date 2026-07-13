import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  unary: vi.fn(),
  pollAndSign: vi.fn(async () => "jti"),
  registerPendedOp: vi.fn(),
}));

vi.mock("./connect", () => ({
  unary: (...args: unknown[]) => mocks.unary(...args),
  DEV_TOKEN: "",
}));
vi.mock("@/auth/session", () => ({ authEnabled: () => true }));
vi.mock("@/auth/intent", () => ({
  pollAndSign: mocks.pollAndSign,
  registerPendedOp: mocks.registerPendedOp,
  clearPendedOp: vi.fn(),
  requireSessionSigningKeys: vi.fn(async () => ({
    privateKey: {} as CryptoKey,
    publicKey: {} as CryptoKey,
  })),
}));

import { createSpawn } from "./spawnlet";

describe("createSpawn intent correspondence", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.unary.mockResolvedValue({ spawnId: "sp1" });
  });

  it("omits empty UI bindings from the pended tuple so the CP can resolve app defaults", async () => {
    await createSpawn("spawnery/secret-app", "model-1");

    expect(mocks.unary).toHaveBeenCalledWith("CreateSpawn", expect.objectContaining({ mounts: [] }));
    expect(mocks.registerPendedOp).toHaveBeenCalledWith({
      op: "create-spawn",
      spawnId: "sp1",
      model: "model-1",
      image: "",
      mounts: undefined,
    });
  });

  it("keeps explicit UI bindings pinned in the pended tuple", async () => {
    const mounts = [{ name: "repo", backendUri: "github:octocat/hello", createIfMissing: true }];

    await createSpawn("spawnery/github-app", "model-1", "image-1", "", "", mounts);

    expect(mocks.unary).toHaveBeenCalledWith("CreateSpawn", expect.objectContaining({ mounts }));
    expect(mocks.registerPendedOp).toHaveBeenCalledWith({
      op: "create-spawn",
      spawnId: "sp1",
      model: "model-1",
      image: "image-1",
      mounts,
    });
  });
});
