// Regression test for sp-rxvb: the /ws/session bind frame MUST carry a session-open SignedIntent
// when auth is enabled, or an enforced node NACKs MISSING_INTENT and the client never attaches
// (blank terminal/chat panel). See sessionBind.ts for the full writeup.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const { buildSessionOpenSignedIntentB64Mock, listSpawnsMock } = vi.hoisted(() => ({
  buildSessionOpenSignedIntentB64Mock: vi.fn(async () => "signed-intent-b64"),
  listSpawnsMock: vi.fn(),
}));

vi.mock("@/auth/intent", () => ({
  buildSessionOpenSignedIntentB64: buildSessionOpenSignedIntentB64Mock,
}));
vi.mock("@/api/spawnlet", () => ({
  listSpawns: listSpawnsMock,
}));
vi.mock("@/auth/keypair", () => ({
  getOrCreateSessionKey: vi.fn(async () => ({
    privateKey: {} as CryptoKey,
    publicKey: {} as CryptoKey,
  })),
}));

let authEnabledValue = false;
vi.mock("@/auth/session", () => ({
  authEnabled: () => authEnabledValue,
  getAccessToken: () => "tok",
  useSessionStore: { getState: () => ({ keyStore: {} }) },
}));

import { buildSessionBindFrame } from "./sessionBind";

describe("buildSessionBindFrame", () => {
  beforeEach(() => {
    authEnabledValue = false;
    buildSessionOpenSignedIntentB64Mock.mockClear();
    listSpawnsMock.mockReset();
    listSpawnsMock.mockResolvedValue([{ spawnId: "sp1", generation: 3n }]);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("omits signedIntent when auth is disabled (dev path unchanged)", async () => {
    authEnabledValue = false;
    const frame = await buildSessionBindFrame("sp1", "0", "client-1", 0);
    expect(frame).toEqual({ spawnId: "sp1", sessionId: "0", clientId: "client-1", token: "tok", cursor: 0 });
    expect(buildSessionOpenSignedIntentB64Mock).not.toHaveBeenCalled();
  });

  it("signs a session-open intent over the spawn's live generation when auth is enabled", async () => {
    authEnabledValue = true;
    const frame = await buildSessionBindFrame("sp1", "2", "client-1", 5);
    expect(frame.signedIntent).toBe("signed-intent-b64");
    expect(buildSessionOpenSignedIntentB64Mock).toHaveBeenCalledWith(
      "sp1",
      "2",
      3n, // generation from listSpawns, not a hardcoded/stale value
      expect.anything(),
      expect.anything(),
    );
    // Base fields still present alongside the intent.
    expect(frame.spawnId).toBe("sp1");
    expect(frame.sessionId).toBe("2");
    expect(frame.clientId).toBe("client-1");
    expect(frame.cursor).toBe(5);
  });

  it("defaults sessionId to '0' in the signed intent when unset", async () => {
    authEnabledValue = true;
    await buildSessionBindFrame("sp1", "", "client-1", 0);
    expect(buildSessionOpenSignedIntentB64Mock).toHaveBeenCalledWith("sp1", "0", 3n, expect.anything(), expect.anything());
  });

  it("falls back to generation 0 when the spawn isn't found in listSpawns", async () => {
    authEnabledValue = true;
    listSpawnsMock.mockResolvedValue([{ spawnId: "other-spawn", generation: 9n }]);
    await buildSessionBindFrame("sp1", "0", "client-1", 0);
    expect(buildSessionOpenSignedIntentB64Mock).toHaveBeenCalledWith("sp1", "0", 0n, expect.anything(), expect.anything());
  });

  it("does not block the bind when signing fails (best-effort: unsigned bind still sent)", async () => {
    authEnabledValue = true;
    buildSessionOpenSignedIntentB64Mock.mockRejectedValueOnce(new Error("boom"));
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const frame = await buildSessionBindFrame("sp1", "0", "client-1", 0);
    expect(frame.signedIntent).toBeUndefined();
    expect(frame.spawnId).toBe("sp1");
    errSpy.mockRestore();
  });
});
