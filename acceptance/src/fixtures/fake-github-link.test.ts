import { describe, expect, it, vi } from "vitest";
import type { AuthStrategy } from "../auth/types";
import type { Identity } from "./identity-pool";
import { bootstrapFakeGitHubLinks } from "./fake-github-link";

const identities: Identity[] = [
  { token: "alice", owner: "ignored-alice" },
  { token: "bob", owner: "ignored-bob" },
];

function redirect(location?: string): Response {
  return new Response(null, {
    status: 302,
    headers: location ? { Location: location } : undefined,
  });
}

function fakeAuth(): Pick<AuthStrategy, "cpAccessToken" | "accountId"> {
  return {
    cpAccessToken: vi.fn(async (identity: Identity) => `bearer-${identity.token}`),
    accountId: vi.fn(async (identity: Identity) => `account-${identity.token}`),
  };
}

describe("bootstrapFakeGitHubLinks", () => {
  it("links identities sequentially through start, authorize, callback, and redeem", async () => {
    const events: string[] = [];
    const auth = fakeAuth();
    const fetchImpl = vi.fn(async (input: string | URL, init?: RequestInit): Promise<Response> => {
      const url = new URL(String(input));
      const headers = new Headers(init?.headers);
      if (url.pathname === "/github/link/start") {
        const login = headers.get("Authorization")?.replace("Bearer bearer-", "") ?? "";
        events.push(`${login}:start`);
        expect(init?.method).toBe("POST");
        expect(headers.get("Authorization")).toBe(`Bearer bearer-${login}`);
        expect(JSON.parse(String(init?.body))).toEqual({ client_kind: "device", port: 0, host: "github.com" });
        return new Response(JSON.stringify({
          authorize_url: `http://fake-idp.example/login/oauth/authorize?flow=${login}`,
          flow_id: `flow-${login}`,
        }));
      }
      if (url.hostname === "fake-idp.example") {
        const login = url.searchParams.get("flow") ?? "";
        events.push(`${login}:authorize`);
        expect(init?.redirect).toBe("manual");
        expect(url.searchParams.get("login_hint")).toBe(login);
        return redirect(`https://as.example/github/link/callback?code=code-${login}&state=state-${login}`);
      }
      if (url.pathname === "/github/link/callback") {
        const login = url.searchParams.get("code")?.replace("code-", "") ?? "";
        events.push(`${login}:callback`);
        expect(init?.redirect).toBe("manual");
        return new Response("linked", { status: 200 });
      }
      if (url.pathname === "/github/link/redeem") {
        const login = headers.get("Authorization")?.replace("Bearer bearer-", "") ?? "";
        events.push(`${login}:redeem`);
        expect(init?.method).toBe("POST");
        expect(headers.get("Authorization")).toBe(`Bearer bearer-${login}`);
        expect(JSON.parse(String(init?.body))).toEqual({
          flow_id: `flow-${login}`,
          rc: "",
          confirm_switch: false,
        });
        return new Response(JSON.stringify({
          secret_id: `gh:account-${login}`,
          host: "github.com",
          login,
          status: "linked",
          access_token: "ignored-metadata-projection",
        }));
      }
      throw new Error(`unexpected URL ${url}`);
    }) as unknown as typeof fetch;

    await bootstrapFakeGitHubLinks({ asOrigin: "https://as.example/", identities, auth, fetchImpl });

    expect(events).toEqual([
      "alice:start", "alice:authorize", "alice:callback", "alice:redeem",
      "bob:start", "bob:authorize", "bob:callback", "bob:redeem",
    ]);
  });

  it.each([
    ["secret_id", { secret_id: "gh:someone-else", host: "github.com", login: "alice", status: "linked" }],
    ["host", { secret_id: "gh:account-alice", host: "elsewhere.example", login: "alice", status: "linked" }],
    ["login", { secret_id: "gh:account-alice", host: "github.com", login: "someone-else", status: "linked" }],
    ["status", { secret_id: "gh:account-alice", host: "github.com", login: "alice", status: "pending" }],
  ])("rejects incoherent %s metadata", async (_field, metadata) => {
    let call = 0;
    const fetchImpl = vi.fn(async (): Promise<Response> => {
      call += 1;
      if (call === 1) return new Response(JSON.stringify({ authorize_url: "https://fake.example/auth", flow_id: "flow" }));
      if (call === 2) return redirect("https://as.example/github/link/callback");
      if (call === 3) return new Response(null, { status: 200 });
      return new Response(JSON.stringify(metadata));
    }) as unknown as typeof fetch;
    await expect(bootstrapFakeGitHubLinks({
      asOrigin: "https://as.example",
      identities: [identities[0]],
      auth: fakeAuth(),
      fetchImpl,
    })).rejects.toThrow(new RegExp(String(_field)));
  });

  it.each([
    ["authorize", 401, "authorize rejected"],
    ["authorize Location", 302, ""],
    ["callback", 503, "callback rejected"],
    ["redeem", 409, "redeem rejected"],
  ])("reports bounded %s failures without credentials", async (stage, status, body) => {
    const secret = "bearer-alice";
    let call = 0;
    const fetchImpl = vi.fn(async (): Promise<Response> => {
      call += 1;
      if (call === 1) return new Response(JSON.stringify({ authorize_url: "https://fake.example/auth", flow_id: "flow" }));
      if (stage === "authorize") return new Response(`${body}${"x".repeat(70 * 1024)}${secret}`, { status });
      if (stage === "authorize Location") return redirect();
      if (call === 2) return redirect("https://as.example/github/link/callback");
      if (stage === "callback") return new Response(`${body}${"x".repeat(70 * 1024)}${secret}`, { status });
      if (call === 3) return new Response(null, { status: 200 });
      return new Response(`${body}${"x".repeat(70 * 1024)}${secret}`, { status });
    }) as unknown as typeof fetch;

    const promise = bootstrapFakeGitHubLinks({
      asOrigin: "https://as.example",
      identities: [identities[0]],
      auth: fakeAuth(),
      fetchImpl,
    });
    const error = await promise.catch((caught: unknown) => caught as Error);
    expect(error).toBeInstanceOf(Error);
    if (!(error instanceof Error)) throw new Error("expected bootstrap to fail");
    expect(error.message).toContain(stage.split(" ")[0]);
    expect(error.message).toContain(String(status));
    expect(error.message.length).toBeLessThan(66 * 1024);
    expect(error.message).not.toContain(secret);
  });
});
