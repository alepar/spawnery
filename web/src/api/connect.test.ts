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
  clearSessionKey: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("@/auth/refresh", () => ({
  refreshAccessToken: vi.fn().mockResolvedValue({ kind: "error", message: "not reached" }),
}));

// ── Helpers ──────────────────────────────────────────────────────────────────

import { useSessionStore } from "@/auth/session";
import { MemoryKeyStore } from "@/auth/keystore";
import { unary, tryRefresh } from "./connect";
import * as keypairMod from "@/auth/keypair";
import * as refreshMod from "@/auth/refresh";

beforeEach(() => {
  vi.mocked(refreshMod.refreshAccessToken).mockClear();
  vi.mocked(keypairMod.loadSessionKey).mockResolvedValue(null);
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

describe("unary — audience separation", () => {
  it("puts only the CP token in Authorization", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("{}", {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }));
    vi.stubGlobal("fetch", fetchMock);
    await unary("ListSpawns", {});
    const init = fetchMock.mock.calls[0][1] as RequestInit;
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer cp-token");
    expect(JSON.stringify(init)).not.toContain("node-token");
    vi.unstubAllGlobals();
  });
});

describe("tryRefresh — auth incarnation fencing", () => {
  function delayedRefresh() {
    let resolve!: (value: { kind: "ok"; cpAccessToken: string; nodeAccessToken: string; refreshTokenHash: string; expiresAt: bigint }) => void;
    const result = new Promise<Parameters<typeof resolve>[0]>((r) => { resolve = r; });
    vi.mocked(keypairMod.loadSessionKey).mockResolvedValue({
      privateKey: {} as CryptoKey,
      publicKey: {} as CryptoKey,
    });
    vi.mocked(keypairMod.exportSpkiDer).mockResolvedValue(new Uint8Array([1]));
    vi.mocked(refreshMod.refreshAccessToken).mockImplementationOnce(() => result);
    return resolve;
  }

  it.each(["logout", "key-loss"])("discards a delayed result after %s", async (transition) => {
    const resolve = delayedRefresh();
    const pending = tryRefresh();
    await vi.waitFor(() => expect(refreshMod.refreshAccessToken).toHaveBeenCalled());
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 200 })));
    if (transition === "logout") await useSessionStore.getState().logout();
    else await useSessionStore.getState().recoverKeyLoss();
    resolve({ kind: "ok", cpAccessToken: "cp-stale", nodeAccessToken: "node-stale", refreshTokenHash: "rth-stale", expiresAt: 9n });
    await expect(pending).resolves.toBe(false);
    expect(useSessionStore.getState().cpAccessToken).toBe("");
    expect(useSessionStore.getState().nodeAccessToken).toBe("");
    vi.unstubAllGlobals();
  });

  it("does not let an old result overwrite a relogged session", async () => {
    const resolve = delayedRefresh();
    const pending = tryRefresh();
    await vi.waitFor(() => expect(refreshMod.refreshAccessToken).toHaveBeenCalled());
    const epoch = useSessionStore.getState().authEpoch + 1;
    useSessionStore.setState({
      authEpoch: epoch, status: "authed", cpAccessToken: "cp-current", nodeAccessToken: "node-current",
    });
    resolve({ kind: "ok", cpAccessToken: "cp-stale", nodeAccessToken: "node-stale", refreshTokenHash: "rth-stale", expiresAt: 9n });
    await expect(pending).resolves.toBe(false);
    expect(useSessionStore.getState()).toEqual(expect.objectContaining({
      authEpoch: epoch, cpAccessToken: "cp-current", nodeAccessToken: "node-current",
    }));
  });
});
