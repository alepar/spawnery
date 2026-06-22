// Regression test for the resume deadlock: ResumeSpawn (and recreate/migrate) block server-side
// awaiting the client's SignedIntent, so the client MUST run pollAndSign concurrently with the
// blocking RPC — not after it returns (which CreateSpawn does, because CreateSpawn is async).
// See cmd/spawnctl/intent.go:54 and cmd/spawnctl/resume.go:3 for the contract.
import { describe, it, expect, vi, afterEach } from "vitest";

const hoisted = vi.hoisted(() => {
  let resolveSigned!: () => void;
  const signedGate = new Promise<void>((r) => {
    resolveSigned = r;
  });
  // The pollAndSign mock fires the gate, standing in for the real GetPendingIntent→SubmitIntent
  // round-trip that unblocks the CP's blocking lifecycle RPC.
  const pollAndSign = vi.fn(async () => {
    resolveSigned();
    return "jti";
  });
  return { signedGate, pollAndSign };
});

vi.mock("@/auth/intent", () => ({
  pollAndSign: hoisted.pollAndSign,
  registerPendedOp: vi.fn(),
  clearPendedOp: vi.fn(),
}));
vi.mock("@/auth/session", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/session")>();
  return { ...actual, authEnabled: () => true };
});
vi.mock("@/auth/keypair", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/keypair")>();
  return {
    ...actual,
    getOrCreateSessionKey: vi.fn(async () => ({
      privateKey: {} as CryptoKey,
      publicKey: {} as CryptoKey,
    })),
  };
});

import { resumeSpawn } from "./spawnlet";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("resumeSpawn intent handshake", () => {
  it("runs pollAndSign concurrently with the blocking ResumeSpawn RPC (no deadlock)", async () => {
    (globalThis as any).fetch = vi.fn(async (url: string) => {
      if (String(url).includes("ResumeSpawn")) {
        // The CP blocks ResumeSpawn until the client submits the signed intent.
        await hoisted.signedGate;
      }
      return { ok: true, json: async () => ({}), text: async () => "" } as any;
    });

    await Promise.race([
      resumeSpawn("s1"),
      new Promise<never>((_, reject) =>
        setTimeout(
          () => reject(new Error("deadlock: ResumeSpawn awaited before pollAndSign started")),
          500,
        ),
      ),
    ]);

    expect(hoisted.pollAndSign).toHaveBeenCalledTimes(1);
  });
});
