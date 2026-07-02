import { describe, it, expect, vi, afterEach } from "vitest";
import {
  establishOAuthSession,
  refreshOAuthSession,
  ACCESS_TOKEN_TTL_MS,
  type OAuthSessionState,
} from "./oauth-session";
import { toBase64Url } from "./pop";

afterEach(() => {
  vi.unstubAllGlobals();
});

function redirectResponse(location: string, cookies: string[] = []): Response {
  const headers: [string, string][] = [["location", location]];
  for (const c of cookies) headers.push(["set-cookie", c]);
  return new Response(null, { status: 302, headers });
}

describe("establishOAuthSession", () => {
  it("drives authorize -> IdP(with login_hint) -> callback and extracts the session", async () => {
    let capturedLoginHint = "";
    const fetchMock = vi.fn(async (input: string | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        expect(url.searchParams.get("session_pubkey")).toBeTruthy();
        expect(url.searchParams.get("redirect_uri")).toBe("https://web.example/callback");
        return redirectResponse(
          "http://fake-idp.example:9099/login/oauth/authorize?client_id=x&redirect_uri=y&state=as-internal",
          ["as_flow=flow-abc; Path=/; HttpOnly"],
        );
      }
      if (url.hostname === "fake-idp.example") {
        capturedLoginHint = url.searchParams.get("login_hint") ?? "";
        return redirectResponse("https://as.example/oauth/callback?code=code123&state=as-internal");
      }
      const state = new URL(String(fetchMock.mock.calls[0][0])).searchParams.get("state");
      return redirectResponse(
        `https://web.example/callback?access_token=tok1&state=${state}&refresh_token_hash=${toBase64Url(new Uint8Array(32).fill(7))}`,
        ["refresh_token=raw-rt-1; Path=/refresh; HttpOnly; Secure; SameSite=Strict"],
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const before = Date.now();
    const session = await establishOAuthSession({
      asOrigin: "https://as.example",
      redirectUri: "https://web.example/callback",
      loginHint: "alice",
    });

    expect(capturedLoginHint).toBe("alice");
    expect(session.accessToken).toBe("tok1");
    expect(session.refreshTokenRaw).toBe("raw-rt-1");
    expect(session.refreshTokenHash).toEqual(new Uint8Array(32).fill(7));
    expect(session.expiresAt).toBeGreaterThanOrEqual(before + ACCESS_TOKEN_TTL_MS);
    expect(session.privateKey).toBeDefined();
    expect(session.publicKey).toBeDefined();
  });

  it("forwards the as_flow cookie captured from /oauth/authorize to /oauth/callback", async () => {
    let cookieSeenAtCallback = "";
    const fetchMock = vi.fn(async (input: string | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        return redirectResponse("http://fake-idp.example/login/oauth/authorize", ["as_flow=flow-xyz; Path=/"]);
      }
      if (url.hostname === "fake-idp.example") {
        return redirectResponse("https://as.example/oauth/callback?code=c&state=s");
      }
      cookieSeenAtCallback = new Headers(init?.headers).get("Cookie") ?? "";
      const state = new URL(String(fetchMock.mock.calls[0][0])).searchParams.get("state");
      return redirectResponse(
        `https://web.example/callback?access_token=t&state=${state}&refresh_token_hash=${toBase64Url(new Uint8Array(32))}`,
        ["refresh_token=r; Path=/refresh"],
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    await establishOAuthSession({
      asOrigin: "https://as.example",
      redirectUri: "https://web.example/callback",
      loginHint: "",
    });
    expect(cookieSeenAtCallback).toBe("as_flow=flow-xyz");
  });

  it("does not append login_hint when empty (falls back to the fake's default user)", async () => {
    let sawLoginHintParam = true;
    const fetchMock = vi.fn(async (input: string | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        return redirectResponse("http://fake-idp.example/login/oauth/authorize", ["as_flow=f; Path=/"]);
      }
      if (url.hostname === "fake-idp.example") {
        sawLoginHintParam = url.searchParams.has("login_hint");
        return redirectResponse("https://as.example/oauth/callback?code=c&state=s");
      }
      const state = new URL(String(fetchMock.mock.calls[0][0])).searchParams.get("state");
      return redirectResponse(
        `https://web.example/callback?access_token=t&state=${state}&refresh_token_hash=${toBase64Url(new Uint8Array(32))}`,
        ["refresh_token=r; Path=/refresh"],
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    await establishOAuthSession({
      asOrigin: "https://as.example",
      redirectUri: "https://web.example/callback",
      loginHint: "",
    });
    expect(sawLoginHintParam).toBe(false);
  });

  it("throws on a non-302 /oauth/authorize response", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("bad", { status: 400 })));
    await expect(
      establishOAuthSession({
        asOrigin: "https://as.example",
        redirectUri: "https://web.example/callback",
        loginHint: "",
      }),
    ).rejects.toThrow(/oauth\/authorize.*expected 302/);
  });

  it("throws when the callback redirects with an error param", async () => {
    const fetchMock = vi.fn(async (input: string | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        return redirectResponse("http://fake-idp.example/login/oauth/authorize", ["as_flow=f; Path=/"]);
      }
      if (url.hostname === "fake-idp.example") {
        return redirectResponse("https://as.example/oauth/callback?code=c&state=s");
      }
      return redirectResponse("https://web.example/callback?error=access_denied&error_description=nope");
    });
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      establishOAuthSession({
        asOrigin: "https://as.example",
        redirectUri: "https://web.example/callback",
        loginHint: "",
      }),
    ).rejects.toThrow(/error=access_denied/);
  });

  it("throws on a state mismatch", async () => {
    const fetchMock = vi.fn(async (input: string | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        return redirectResponse("http://fake-idp.example/login/oauth/authorize", ["as_flow=f; Path=/"]);
      }
      if (url.hostname === "fake-idp.example") {
        return redirectResponse("https://as.example/oauth/callback?code=c&state=s");
      }
      return redirectResponse(
        `https://web.example/callback?access_token=t&state=WRONG&refresh_token_hash=${toBase64Url(new Uint8Array(32))}`,
        ["refresh_token=r; Path=/refresh"],
      );
    });
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      establishOAuthSession({
        asOrigin: "https://as.example",
        redirectUri: "https://web.example/callback",
        loginHint: "",
      }),
    ).rejects.toThrow(/state mismatch/);
  });

  it("throws when the callback response sets no refresh_token cookie", async () => {
    const fetchMock = vi.fn(async (input: string | URL): Promise<Response> => {
      const url = new URL(String(input));
      if (url.pathname === "/oauth/authorize") {
        return redirectResponse("http://fake-idp.example/login/oauth/authorize", ["as_flow=f; Path=/"]);
      }
      if (url.hostname === "fake-idp.example") {
        return redirectResponse("https://as.example/oauth/callback?code=c&state=s");
      }
      const state = new URL(String(fetchMock.mock.calls[0][0])).searchParams.get("state");
      return redirectResponse(
        `https://web.example/callback?access_token=t&state=${state}&refresh_token_hash=${toBase64Url(new Uint8Array(32))}`,
      );
    });
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      establishOAuthSession({
        asOrigin: "https://as.example",
        redirectUri: "https://web.example/callback",
        loginHint: "",
      }),
    ).rejects.toThrow(/no refresh_token cookie/);
  });
});

describe("refreshOAuthSession", () => {
  async function makeState(): Promise<OAuthSessionState> {
    const kp = (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, [
      "sign",
      "verify",
    ])) as CryptoKeyPair;
    return {
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      accessToken: "old-token",
      refreshTokenRaw: "old-raw",
      refreshTokenHash: new Uint8Array(32).fill(1),
      expiresAt: Date.now() - 1000,
    };
  }

  it("POSTs PoP headers + the refresh_token cookie and rotates the session", async () => {
    const prev = await makeState();
    let seenCookie = "";
    const fetchMock = vi.fn(async (input: string | URL, init?: RequestInit): Promise<Response> => {
      expect(String(input)).toBe("https://as.example/refresh");
      expect(init?.method).toBe("POST");
      const headers = new Headers(init?.headers);
      seenCookie = headers.get("Cookie") ?? "";
      expect(headers.get("X-PoP-Timestamp")).toBeTruthy();
      expect(headers.get("X-PoP-Nonce")).toBeTruthy();
      expect(headers.get("X-PoP-Sig")).toBeTruthy();
      return new Response(
        JSON.stringify({ access_token: "new-token", refresh_token_hash: toBase64Url(new Uint8Array(32).fill(2)) }),
        { status: 200, headers: [["set-cookie", "refresh_token=new-raw; Path=/refresh; HttpOnly"]] },
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    const before = Date.now();
    const next = await refreshOAuthSession({ asOrigin: "https://as.example" }, prev);

    expect(seenCookie).toBe("refresh_token=old-raw");
    expect(next.accessToken).toBe("new-token");
    expect(next.refreshTokenRaw).toBe("new-raw");
    expect(next.refreshTokenHash).toEqual(new Uint8Array(32).fill(2));
    expect(next.expiresAt).toBeGreaterThanOrEqual(before + ACCESS_TOKEN_TTL_MS);
    expect(next.privateKey).toBe(prev.privateKey); // same session key carried forward
  });

  it("throws on a non-ok response", async () => {
    const prev = await makeState();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 401 })));
    await expect(refreshOAuthSession({ asOrigin: "https://as.example" }, prev)).rejects.toThrow(
      /refresh failed: 401/,
    );
  });

  it("throws when the response sets no refresh_token cookie", async () => {
    const prev = await makeState();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ access_token: "t", refresh_token_hash: toBase64Url(new Uint8Array(32)) }), {
          status: 200,
        }),
      ),
    );
    await expect(refreshOAuthSession({ asOrigin: "https://as.example" }, prev)).rejects.toThrow(
      /no refresh_token cookie/,
    );
  });
});
