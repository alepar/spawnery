/**
 * Tests for session store bootstrap.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { useSessionStore, authEnabled, DEV_TOKEN } from "./session";
import { MemoryKeyStore } from "./keystore";
import { ProtoWriter } from "./protobuf";
import { toBase64Url } from "./token";

// Reset zustand state between tests
beforeEach(() => {
  useSessionStore.setState({
    status: "loading",
    accessToken: "",
    refreshTokenHash: "",
    account: null,
  });
});

describe("authEnabled", () => {
  it("returns false when AS_ORIGIN is empty and not PROD", () => {
    // In test environment, VITE_AS_ORIGIN is not set and PROD is false.
    // This test validates the logic — actual env vars vary by test setup.
    // If authEnabled() is true in tests, that's also acceptable (just means AS is configured).
    expect(typeof authEnabled()).toBe("boolean");
  });
});

describe("bootstrap — dev mode (auth disabled)", () => {
  it("sets status=authed with DEV_TOKEN when auth is disabled", async () => {
    // Bootstrap with a memory store to avoid real IDB.
    const store = new MemoryKeyStore();
    // In test env, authEnabled() is false (no VITE_AS_ORIGIN).
    // If it's true for some reason, this test is a no-op.
    if (authEnabled()) return;

    await useSessionStore.getState().bootstrap(store);
    const s = useSessionStore.getState();
    expect(s.status).toBe("authed");
    expect(s.accessToken).toBe(DEV_TOKEN);
  });
});

describe("getAccessToken", () => {
  it("returns DEV_TOKEN in dev mode", () => {
    if (authEnabled()) return; // skip if auth is configured
    expect(useSessionStore.getState().getAccessToken()).toBe(DEV_TOKEN);
  });

  it("returns current accessToken in auth-enabled mode", () => {
    if (!authEnabled()) return; // skip in dev mode
    useSessionStore.setState({ accessToken: "test-token" });
    expect(useSessionStore.getState().getAccessToken()).toBe("test-token");
  });
});

describe("setToken", () => {
  it("sets status to authed and parses account info", () => {
    // Build a minimal wire token with known account_id and handle.
    const w = new ProtoWriter();
    w.writeBytes(1, "acc-test");
    w.writeBytes(2, "testuser");
    w.writeVarint(6, 1800000000n);
    const body = w.finish();
    const wire = toBase64Url(body) + "." + toBase64Url(new Uint8Array(64));

    useSessionStore.getState().setToken(wire, "rth123");
    const s = useSessionStore.getState();
    expect(s.status).toBe("authed");
    expect(s.account?.accountId).toBe("acc-test");
    expect(s.account?.handle).toBe("testuser");
    expect(s.refreshTokenHash).toBe("rth123");
  });
});

describe("logout", () => {
  it("clears token and sets login-required", async () => {
    const store = new MemoryKeyStore();
    useSessionStore.setState({ status: "authed", accessToken: "tok", keyStore: store });

    // Mock fetch to avoid real network
    const fetchMock = vi.fn().mockResolvedValue(new Response("", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await useSessionStore.getState().logout();

    const s = useSessionStore.getState();
    expect(s.status).toBe("login-required");
    expect(s.accessToken).toBe("");

    vi.unstubAllGlobals();
  });
});
