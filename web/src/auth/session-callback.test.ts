/**
 * Tests for bootstrap() callback path — AS error code stored in the session store
 * and post-login route restoration via browserHistory.replaceState.
 *
 * These tests run with auth enabled (AS_ORIGIN mocked non-empty) and control
 * parseCallback directly to exercise the code paths that are unreachable in
 * the regular session.test.ts (which runs in dev/no-auth mode).
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import type { CallbackResult } from "./oauth";

// ── Module mocks (hoisted) ───────────────────────────────────────────────────

// Make authEnabled() return true by giving AS_ORIGIN a non-empty value.
vi.mock("@/config/endpoints", () => ({
  AS_ORIGIN: "https://as.example.com",
  asHttpUrl: (path: string) => `https://as.example.com${path}`,
  cpHttpUrl: (path: string) => path,
  cpWsUrl: (path: string) => `ws://localhost${path}`,
}));

const parseCallbackMock = vi.fn<() => CallbackResult>(() => ({ kind: "none" }));
const replaceStateMock = vi.fn<(url: string) => void>();

vi.mock("./oauth", () => ({
  parseCallback: () => parseCallbackMock(),
  sessionStateStorage: { get: vi.fn(), set: vi.fn(), remove: vi.fn() },
  browserHistory: {
    replaceState: (url: string) => replaceStateMock(url),
    locationSearch: vi.fn().mockReturnValue(""),
    locationPathname: vi.fn().mockReturnValue("/callback"),
  },
}));

// Prevent real keypair/IDB operations during bootstrap's refresh fallback.
vi.mock("./keypair", () => ({
  getOrCreateSessionKey: vi.fn().mockRejectedValue(new Error("no keystore in test")),
  exportSpkiDer: vi.fn(),
  sessionKeyHash: vi.fn(),
  clearSessionKey: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("./refresh", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./refresh")>();
  return {
    ...actual,
    refreshAccessToken: vi.fn().mockResolvedValue({ kind: "network-error" }),
  };
});

// ── Helpers ──────────────────────────────────────────────────────────────────

// Import after mocks are registered.
import { useSessionStore } from "./session";
import { MemoryKeyStore } from "./keystore";

beforeEach(() => {
  useSessionStore.setState({
    status: "loading",
    accessToken: "",
    refreshTokenHash: "",
    account: null,
    callbackErrorCode: null,
  });
  parseCallbackMock.mockReturnValue({ kind: "none" });
  replaceStateMock.mockClear();
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("bootstrap — AS error callback stored in state", () => {
  it("sets callbackErrorCode=registration_closed and status=login-required", async () => {
    parseCallbackMock.mockReturnValue({
      kind: "error",
      code: "registration_closed",
      description: "Registrations are closed.",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    const s = useSessionStore.getState();
    expect(s.status).toBe("login-required");
    expect(s.callbackErrorCode).toBe("registration_closed");
  });

  it("sets callbackErrorCode=access_denied on access_denied error", async () => {
    parseCallbackMock.mockReturnValue({
      kind: "error",
      code: "access_denied",
      description: "",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    const s = useSessionStore.getState();
    expect(s.status).toBe("login-required");
    expect(s.callbackErrorCode).toBe("access_denied");
  });

  it("sets callbackErrorCode=unknown for unrecognised error codes", async () => {
    parseCallbackMock.mockReturnValue({
      kind: "error",
      code: "some_future_code",
      description: "",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().callbackErrorCode).toBe("unknown");
  });
});

describe("bootstrap — success callback restores original route", () => {
  // Build a minimal wire token (valid enough for parseAccessToken to skip gracefully).
  const FAKE_TOKEN = "AAAA.AAAA"; // invalid but setToken ignores parse errors

  it("calls browserHistory.replaceState with cb.route on ok callback", async () => {
    parseCallbackMock.mockReturnValue({
      kind: "ok",
      accessToken: FAKE_TOKEN,
      refreshTokenHash: "",
      route: "/spawn/abc123",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().status).toBe("authed");
    expect(replaceStateMock).toHaveBeenCalledWith("/spawn/abc123");
  });

  it("does not call replaceState when route is empty", async () => {
    parseCallbackMock.mockReturnValue({
      kind: "ok",
      accessToken: FAKE_TOKEN,
      refreshTokenHash: "",
      route: "",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().status).toBe("authed");
    // Empty route: replaceState should NOT be called (falsy guard prevents it).
    expect(replaceStateMock).not.toHaveBeenCalled();
  });

  it("clears callbackErrorCode on successful token set", async () => {
    // Pre-seed an error code to verify setToken clears it.
    useSessionStore.setState({ callbackErrorCode: "access_denied" });

    parseCallbackMock.mockReturnValue({
      kind: "ok",
      accessToken: FAKE_TOKEN,
      refreshTokenHash: "",
      route: "/templates",
    });

    const store = new MemoryKeyStore();
    await useSessionStore.getState().bootstrap(store);

    expect(useSessionStore.getState().callbackErrorCode).toBeNull();
    expect(useSessionStore.getState().status).toBe("authed");
  });
});
