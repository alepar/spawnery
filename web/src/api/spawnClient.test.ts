/**
 * Tests for spawnClient.ts's AuthProvider.refresh — asserts it mirrors unary()'s
 * (api/connect.ts) session-status handling on a failed refresh (login-required parity,
 * see connect.test.ts for the equivalent unary() coverage).
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

// ── Module mocks (hoisted) ───────────────────────────────────────────────────

vi.mock("@/config/endpoints", () => ({
  CP_ORIGIN: "https://cp.example.com",
  cpHttpUrl: (path: string) => `/cp${path}`,
}));

vi.mock("./connect", () => ({
  tryRefresh: vi.fn(),
}));

// ── Helpers ──────────────────────────────────────────────────────────────────

import { useSessionStore } from "@/auth/session";
import { MemoryKeyStore } from "@/auth/keystore";
import { auth } from "./spawnClient";
import { tryRefresh } from "./connect";

beforeEach(() => {
  vi.mocked(tryRefresh).mockReset();
  const store = new MemoryKeyStore();
  useSessionStore.setState({
    status: "authed",
    cpAccessToken: "cp-token",
    nodeAccessToken: "node-token",
    refreshTokenHash: "rth",
    account: null,
    callbackErrorCode: null,
    keyStore: store,
  });
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("spawnClient's AuthProvider.refresh", () => {
  it("sets status=login-required on a generic refresh failure", async () => {
    vi.mocked(tryRefresh).mockResolvedValue(false);

    await auth.refresh?.();

    expect(useSessionStore.getState().status).toBe("login-required");
  });

  it("does not clobber a more specific status (key-lost) already set by tryRefresh", async () => {
    vi.mocked(tryRefresh).mockImplementation(async () => {
      useSessionStore.getState().setStatus("key-lost");
      return false;
    });

    await auth.refresh?.();

    expect(useSessionStore.getState().status).toBe("key-lost");
  });

  it("leaves status untouched when refresh succeeds", async () => {
    vi.mocked(tryRefresh).mockResolvedValue(true);

    await auth.refresh?.();

    expect(useSessionStore.getState().status).toBe("authed");
  });
});
