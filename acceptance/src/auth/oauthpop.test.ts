import { describe, it, expect, vi, beforeEach } from "vitest";
import { OAuthPoPAuth, type PageLike, type RouteLike } from "./oauthpop";
import type { Identity } from "../fixtures/identity-pool";
import { establishOAuthSession, refreshOAuthSession, type OAuthSessionState } from "./oauth-session";

vi.mock("./oauth-session", () => ({
  establishOAuthSession: vi.fn(),
  refreshOAuthSession: vi.fn(),
}));

const establishMock = vi.mocked(establishOAuthSession);
const refreshMock = vi.mocked(refreshOAuthSession);

function fakeState(accessToken: string, expiresAt: number): OAuthSessionState {
  return {
    privateKey: {} as CryptoKey,
    publicKey: {} as CryptoKey,
    accessToken,
    refreshTokenRaw: "raw-refresh",
    refreshTokenHash: new Uint8Array(32),
    expiresAt,
  };
}

beforeEach(() => {
  establishMock.mockReset();
  refreshMock.mockReset();
});

const cfg = { asOrigin: "https://as.example", webOrigin: "https://web.example" };
const alice: Identity = { token: "alice", owner: "acc-owner-1" };

describe("OAuthPoPAuth.oracleToken", () => {
  it("establishes a session once and reuses it while it has plenty of TTL left", async () => {
    establishMock.mockResolvedValue(fakeState("tok-1", Date.now() + 15 * 60 * 1000));
    const auth = new OAuthPoPAuth(cfg);

    expect(await auth.oracleToken(alice)).toBe("tok-1");
    expect(await auth.oracleToken(alice)).toBe("tok-1");

    expect(establishMock).toHaveBeenCalledTimes(1);
    expect(establishMock).toHaveBeenCalledWith({
      asOrigin: "https://as.example",
      redirectUri: "https://web.example/callback",
      loginHint: "alice",
    });
    expect(refreshMock).not.toHaveBeenCalled();
  });

  it("proactively refreshes once the session is within the margin of expiry", async () => {
    establishMock.mockResolvedValue(fakeState("tok-1", Date.now() + 60 * 1000)); // 1 min left < 2 min margin
    refreshMock.mockResolvedValue(fakeState("tok-2", Date.now() + 15 * 60 * 1000));
    const auth = new OAuthPoPAuth(cfg);

    expect(await auth.oracleToken(alice)).toBe("tok-2");
    expect(refreshMock).toHaveBeenCalledTimes(1);
    expect(establishMock).toHaveBeenCalledTimes(1);
  });

  it("does not refresh again once the refreshed session has plenty of TTL left", async () => {
    establishMock.mockResolvedValue(fakeState("tok-1", Date.now() + 60 * 1000));
    refreshMock.mockResolvedValue(fakeState("tok-2", Date.now() + 15 * 60 * 1000));
    const auth = new OAuthPoPAuth(cfg);

    await auth.oracleToken(alice);
    expect(await auth.oracleToken(alice)).toBe("tok-2");
    expect(refreshMock).toHaveBeenCalledTimes(1);
  });

  it("keys sessions independently per identity — never shares an owner across workers", async () => {
    establishMock.mockImplementation(async ({ loginHint }) => fakeState(`tok-${loginHint}`, Date.now() + 15 * 60 * 1000));
    const auth = new OAuthPoPAuth(cfg);

    expect(await auth.oracleToken({ token: "alice", owner: "o1" })).toBe("tok-alice");
    expect(await auth.oracleToken({ token: "bob", owner: "o2" })).toBe("tok-bob");
    expect(establishMock).toHaveBeenCalledTimes(2);
  });
});

describe("OAuthPoPAuth.cliArgs", () => {
  it("returns -token <accessToken>", async () => {
    establishMock.mockResolvedValue(fakeState("tok-1", Date.now() + 15 * 60 * 1000));
    const auth = new OAuthPoPAuth(cfg);
    expect(await auth.cliArgs(alice)).toEqual(["-token", "tok-1"]);
  });
});

describe("OAuthPoPAuth.seedWeb", () => {
  it("navigates, clicks sign-in, waits for the post-login bounce, and rewrites the fake IdP's login_hint", async () => {
    const auth = new OAuthPoPAuth(cfg);
    let routeHandler: ((route: RouteLike) => Promise<void> | void) | undefined;
    let clickCount = 0;
    const waitForURLMatchers: Array<(url: URL) => boolean> = [];

    const page: PageLike = {
      route: vi.fn(async (pattern, handler) => {
        expect(pattern).toBe("**/login/oauth/authorize*");
        routeHandler = handler;
      }),
      goto: vi.fn(async () => undefined),
      getByTestId: vi.fn((testId: string) => {
        expect(testId).toBe("sign-in-btn");
        return {
          click: async () => {
            clickCount++;
          },
        };
      }),
      waitForURL: vi.fn(async (matcher) => {
        waitForURLMatchers.push(matcher);
      }),
    };

    await auth.seedWeb(page, alice);

    expect(page.goto).toHaveBeenCalledWith("/");
    expect(clickCount).toBe(1);
    expect(waitForURLMatchers).toHaveLength(1);

    // Sanity-check the waitForURL matcher's shape: it stops waiting once the callback query/path
    // has cleared, and keeps waiting while access_token or /callback is still present.
    expect(waitForURLMatchers[0](new URL("https://web.example/callback?access_token=t"))).toBe(false);
    expect(waitForURLMatchers[0](new URL("https://web.example/templates"))).toBe(true);

    // Drive the captured route handler the way Playwright would, and assert it injects login_hint
    // without touching anything else about the request.
    let continuedUrl = "";
    const route: RouteLike = {
      request: () => ({ url: () => "http://fake-idp.example:9099/login/oauth/authorize?client_id=x&state=s" }),
      continue: async (overrides) => {
        continuedUrl = overrides?.url ?? "";
      },
    };
    await routeHandler!(route);
    const rewritten = new URL(continuedUrl);
    expect(rewritten.searchParams.get("login_hint")).toBe("alice");
    expect(rewritten.searchParams.get("client_id")).toBe("x");
    expect(rewritten.origin).toBe("http://fake-idp.example:9099");
  });

  it("does not set login_hint when the identity's token is empty", async () => {
    const auth = new OAuthPoPAuth(cfg);
    let routeHandler: ((route: RouteLike) => Promise<void> | void) | undefined;
    const page: PageLike = {
      route: vi.fn(async (_pattern, handler) => {
        routeHandler = handler;
      }),
      goto: vi.fn(async () => undefined),
      getByTestId: vi.fn(() => ({ click: async () => {} })),
      waitForURL: vi.fn(async () => {}),
    };

    await auth.seedWeb(page, { token: "", owner: "o1" });

    let continuedUrl = "";
    const route: RouteLike = {
      request: () => ({ url: () => "http://fake-idp.example/login/oauth/authorize?client_id=x" }),
      continue: async (overrides) => {
        continuedUrl = overrides?.url ?? "";
      },
    };
    await routeHandler!(route);
    expect(new URL(continuedUrl).searchParams.has("login_hint")).toBe(false);
  });
});
