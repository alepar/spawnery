/**
 * Tests for connect.ts unary — reactive 401 refresh with key-loss detection.
 *
 * _tryRefresh is a private function; we test it via the public unary() path by
 * triggering a 401 response and checking the resulting session status.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

// ── Module mocks (hoisted) ───────────────────────────────────────────────────

vi.mock("@/config/endpoints", () => ({
  AS_ORIGIN: "https://as.example.com",
  asHttpUrl: (path: string) => `https://as.example.com${path}`,
  cpHttpUrl: (path: string) => `/cp${path}`,
  cpWsUrl: (path: string) => `ws://localhost${path}`,
}));

vi.mock("@/auth/keypair", () => ({
  loadSessionKey: vi.fn().mockResolvedValue(null),
  exportSpkiDer: vi.fn(),
  sessionKeyHash: vi.fn().mockResolvedValue(new Uint8Array(32)),
}));

vi.mock("@/auth/refresh", () => ({
  refreshAccessToken: vi.fn().mockResolvedValue({ kind: "error", message: "not reached" }),
}));

// ── Helpers ──────────────────────────────────────────────────────────────────

import { useSessionStore } from "@/auth/session";
import { MemoryKeyStore } from "@/auth/keystore";
import { unary } from "./connect";
import * as keypairMod from "@/auth/keypair";

beforeEach(() => {
  vi.mocked(keypairMod.loadSessionKey).mockResolvedValue(null);
  const store = new MemoryKeyStore();
  useSessionStore.setState({
    status: "authed",
    accessToken: "tok",
    refreshTokenHash: "rth",
    account: null,
    callbackErrorCode: null,
    keyStore: store,
  });
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("unary — 401 with missing key", () => {
  it("sets status=key-lost and throws when key is missing on reactive 401", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("unauthorized", { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await expect(unary("ListSpawns", {})).rejects.toThrow();

    // Status must be key-lost (not clobbered to login-required).
    expect(useSessionStore.getState().status).toBe("key-lost");

    vi.unstubAllGlobals();
  });
});
