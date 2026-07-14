import { beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  unary: vi.fn(),
  pollAndSign: vi.fn(async () => "jti"),
  registerPendedOp: vi.fn(),
  clearPendedOp: vi.fn(),
}));

vi.mock("./connect", () => ({
  unary: (...args: unknown[]) => mocks.unary(...args),
  DEV_TOKEN: "",
}));
vi.mock("@/auth/session", () => ({ authEnabled: () => true }));
vi.mock("@/auth/intent", () => ({
  pollAndSign: mocks.pollAndSign,
  registerPendedOp: mocks.registerPendedOp,
  clearPendedOp: mocks.clearPendedOp,
  requireSessionSigningKeys: vi.fn(async () => ({
    privateKey: {} as CryptoKey,
    publicKey: {} as CryptoKey,
  })),
}));

import { createSpawn, SpawnAuthorizationError } from "./spawnlet";

describe("createSpawn intent correspondence", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.unary.mockResolvedValue({ spawnId: "sp1" });
    mocks.pollAndSign.mockResolvedValue("jti");
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

  it("rejects with the created spawn id when target authorization fails", async () => {
    const cause = new Error("target: certificate signature does not verify");
    mocks.pollAndSign.mockRejectedValueOnce(cause);

    const result = createSpawn("spawnery/secret-app", "model-1");
    await expect(result).rejects.toEqual(
      expect.objectContaining({
        name: "SpawnAuthorizationError",
        spawnId: "sp1",
        message: cause.message,
      }),
    );
    await expect(result).rejects.toBeInstanceOf(SpawnAuthorizationError);
    expect(mocks.clearPendedOp).toHaveBeenCalledWith("sp1");
  });
});
