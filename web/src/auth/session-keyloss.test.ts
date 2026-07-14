/**
 * Tests for the key-loss detection paths in bootstrap() and _doProactiveRefresh().
 *
 * These tests run with auth enabled and parseCallback returning "none" so the
 * code reaches the loadSessionKey call — the exact ITP/storage-eviction scenario
 * spec §5 [AM6] describes.
 *
 * Bootstrap must detect a missing key (loadSessionKey → null) and route to
 * key-lost without minting a fresh keypair.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";

// ── Module mocks (hoisted) ───────────────────────────────────────────────────

vi.mock("@/config/endpoints", () => ({
  AS_ORIGIN: "https://as.example.com",
  asHttpUrl: (path: string) => `https://as.example.com${path}`,
  cpHttpUrl: (path: string) => path,
  cpWsUrl: (path: string) => `ws://localhost${path}`,
}));

vi.mock("./oauth", () => ({
  parseCallback: () => ({ kind: "none" }),
  sessionStateStorage: { get: vi.fn(), set: vi.fn(), remove: vi.fn() },
  browserHistory: {
    replaceState: vi.fn(),
    locationSearch: vi.fn().mockReturnValue(""),
    locationPathname: vi.fn().mockReturnValue("/"),
  },
}));

vi.mock("./keypair", () => ({
  loadSessionKey: vi.fn().mockResolvedValue(null),
  exportSpkiDer: vi.fn(),
  sessionKeyHash: vi.fn(),
  clearSessionKey: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("./refresh", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./refresh")>();
  return {
    ...actual,
    refreshAccessToken: vi.fn().mockResolvedValue({ kind: "error", message: "not reached" }),
  };
});

// ── Helpers ──────────────────────────────────────────────────────────────────

import { useSessionStore, RTH_STORAGE_KEY, _doProactiveRefresh } from "./session";
import { MemoryKeyStore } from "./keystore";
import * as keypairMod from "./keypair";
import * as refreshMod from "./refresh";

beforeEach(() => {
  vi.mocked(keypairMod.loadSessionKey).mockResolvedValue(null);
  localStorage.removeItem(RTH_STORAGE_KEY);
  useSessionStore.setState({
    status: "loading",
    cpAccessToken: "",
    nodeAccessToken: "",
    refreshTokenHash: "",
    account: null,
    callbackErrorCode: null,
  });
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("bootstrap — key missing (ITP/storage eviction)", () => {
  it("sets status=login-required when loadSessionKey returns null AND no RTH (first-run)", async () => {
    // beforeEach already removed RTH_STORAGE_KEY → brand-new visitor scenario.
    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().status).toBe("login-required");
  });

  it("sets status=key-lost when loadSessionKey returns null AND RTH is present (key evicted)", async () => {
    localStorage.setItem(RTH_STORAGE_KEY, "stale-rth");

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().status).toBe("key-lost");
  });

  it("clears the RTH from localStorage on key-lost", async () => {
    localStorage.setItem(RTH_STORAGE_KEY, "stale-rth");

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(localStorage.getItem(RTH_STORAGE_KEY)).toBeNull();
    expect(useSessionStore.getState().status).toBe("key-lost");
  });

  it("does NOT call refreshAccessToken when key is missing", async () => {
    const { refreshAccessToken } = await import("./refresh");
    const refreshMock = vi.mocked(refreshAccessToken);
    refreshMock.mockClear();

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(refreshMock).not.toHaveBeenCalled();
  });
});

describe("proactive refresh — auth incarnation fencing", () => {
  it.each(["logout", "key-loss", "relogin"])("discards a delayed result after %s", async (transition) => {
    let resolve!: (value: { kind: "ok"; cpAccessToken: string; nodeAccessToken: string;
      refreshTokenHash: string; expiresAt: bigint }) => void;
    const result = new Promise<Parameters<typeof resolve>[0]>((r) => { resolve = r; });
    vi.mocked(keypairMod.loadSessionKey).mockResolvedValue({
      privateKey: {} as CryptoKey,
      publicKey: {} as CryptoKey,
    });
    vi.mocked(keypairMod.exportSpkiDer).mockResolvedValue(new Uint8Array([1]));
    vi.mocked(keypairMod.sessionKeyHash).mockResolvedValue(new Uint8Array(32));
    vi.mocked(refreshMod.refreshAccessToken).mockImplementationOnce(() => result);
    useSessionStore.setState({
      authEpoch: 10, status: "authed", cpAccessToken: "cp-old", nodeAccessToken: "node-old",
      keyStore: new MemoryKeyStore(),
    });
    const pending = _doProactiveRefresh();
    await vi.waitFor(() => expect(refreshMod.refreshAccessToken).toHaveBeenCalled());
    if (transition === "logout") {
      useSessionStore.setState({ authEpoch: 11, status: "login-required", cpAccessToken: "", nodeAccessToken: "" });
    } else if (transition === "key-loss") {
      useSessionStore.setState({ authEpoch: 11, status: "key-lost", cpAccessToken: "", nodeAccessToken: "" });
    } else {
      useSessionStore.setState({ authEpoch: 11, status: "authed", cpAccessToken: "cp-new", nodeAccessToken: "node-new" });
    }
    resolve({ kind: "ok", cpAccessToken: "cp-stale", nodeAccessToken: "node-stale",
      refreshTokenHash: "rth-stale", expiresAt: 99n });
    await pending;
    expect(useSessionStore.getState().cpAccessToken).toBe(transition === "relogin" ? "cp-new" : "");
    expect(useSessionStore.getState().nodeAccessToken).toBe(transition === "relogin" ? "node-new" : "");
  });
});
