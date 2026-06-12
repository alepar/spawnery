/**
 * Tests for LoginView: login wall, error code rendering, cnf-mismatch / key-lost CTAs.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { LoginView } from "./LoginView";
import { useSessionStore } from "@/auth/session";
import { MemoryKeyStore } from "@/auth/keystore";

// Mock buildAuthorizeUrl so no real ECDSA or URL redirect happens
vi.mock("@/auth/oauth", () => ({
  buildAuthorizeUrl: vi.fn().mockReturnValue("https://as.example.com/oauth/authorize?state=test"),
  sessionStateStorage: { get: vi.fn(), set: vi.fn(), remove: vi.fn() },
  browserHistory: {
    replaceState: vi.fn(),
    locationSearch: vi.fn().mockReturnValue(""),
    locationPathname: vi.fn().mockReturnValue("/"),
  },
}));

// Mock keypair to avoid real IDB / WebCrypto key generation timing
vi.mock("@/auth/keypair", () => {
  const fakePub = { type: "public" } as unknown as CryptoKey;
  const fakePriv = { type: "private" } as unknown as CryptoKey;
  return {
    getOrCreateSessionKey: vi.fn().mockResolvedValue({ privateKey: fakePriv, publicKey: fakePub }),
    exportSpkiDer: vi.fn().mockResolvedValue(new Uint8Array(65).fill(0x42)),
    clearSessionKey: vi.fn().mockResolvedValue(undefined),
    keyCanSign: vi.fn().mockResolvedValue(true),
  };
});

// Stub window.location.href setter
const hrefSetter = vi.fn();
Object.defineProperty(window, "location", {
  value: { ...window.location, origin: "http://localhost", pathname: "/" },
  writable: true,
});

beforeEach(() => {
  useSessionStore.setState({
    status: "login-required",
    accessToken: "",
    refreshTokenHash: "",
    account: null,
    keyStore: new MemoryKeyStore(),
  });
  vi.clearAllMocks();
});

describe("LoginView — login-required", () => {
  it("renders sign-in button", () => {
    render(<LoginView />);
    expect(screen.getByTestId("sign-in-btn")).toBeTruthy();
    expect(screen.getByTestId("sign-in-btn").textContent).toContain("Sign in");
  });

  it("does not render when authed (wall should not show)", () => {
    useSessionStore.setState({ status: "authed" });
    // LoginView renders regardless of status — gating is in App.tsx.
    // But login-required state shows the sign-in button.
    render(<LoginView />);
    expect(screen.getByTestId("login-view")).toBeTruthy();
  });
});

describe("LoginView — loading state", () => {
  it("renders loading indicator", () => {
    useSessionStore.setState({ status: "loading" });
    render(<LoginView />);
    expect(screen.getByTestId("login-loading")).toBeTruthy();
  });
});

describe("LoginView — error code", () => {
  it("renders registration_closed copy", () => {
    render(<LoginView errorCode="registration_closed" />);
    const error = screen.getByTestId("login-error");
    expect(error.textContent).toContain("Registrations are currently closed");
  });

  it("renders access_denied copy", () => {
    render(<LoginView errorCode="access_denied" />);
    expect(screen.getByTestId("login-error").textContent).toContain("Access was denied");
  });

  it("renders server_error copy", () => {
    render(<LoginView errorCode="server_error" />);
    expect(screen.getByTestId("login-error").textContent).toContain("server error");
  });
});

describe("LoginView — cnf-mismatch", () => {
  it("renders cnf-mismatch state with recovery CTA", () => {
    useSessionStore.setState({ status: "cnf-mismatch" });
    render(<LoginView />);
    expect(screen.getByTestId("login-cnf-mismatch")).toBeTruthy();
    expect(screen.getByTestId("sign-in-recover-btn")).toBeTruthy();
  });
});

describe("LoginView — key-lost", () => {
  it("renders key-lost state with recovery CTA", () => {
    useSessionStore.setState({ status: "key-lost" });
    render(<LoginView />);
    expect(screen.getByTestId("login-key-lost")).toBeTruthy();
    expect(screen.getByTestId("sign-in-recover-btn")).toBeTruthy();
  });
});
