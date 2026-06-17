import { describe, it, expect, vi, beforeEach } from "vitest";
import { startGithubLink, redeemGithubLink, listGithubLinks, revokeGithubLink } from "./githubLink";

function mockFetchOnce(init: { status: number; json?: unknown; text?: string }) {
  return vi.fn().mockResolvedValue({
    ok: init.status >= 200 && init.status < 300,
    status: init.status,
    json: async () => init.json ?? {},
    text: async () => init.text ?? JSON.stringify(init.json ?? {}),
  });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe("startGithubLink", () => {
  it("POSTs client_kind=web with a Bearer header and maps the response", async () => {
    const f = mockFetchOnce({ status: 200, json: { authorize_url: "https://gh/a", flow_id: "flow-1" } });
    vi.stubGlobal("fetch", f);
    const r = await startGithubLink();
    expect(r).toEqual({ authorizeUrl: "https://gh/a", flowId: "flow-1" });
    const [url, opts] = f.mock.calls[0];
    expect(String(url)).toContain("/github/link/start");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toMatchObject({ client_kind: "web" });
    expect(opts.headers.Authorization).toMatch(/^Bearer/);
  });
});

describe("redeemGithubLink", () => {
  it("uses credentials:'include' and sends flow_id + confirm_switch", async () => {
    const f = mockFetchOnce({ status: 200, json: { secret_id: "gh:acct", host: "github.com", login: "octocat", github_user_id: "42", version: 1, updated_at: 7, status: "linked" } });
    vi.stubGlobal("fetch", f);
    const r = await redeemGithubLink("flow-1", true);
    expect(r).toEqual({ kind: "linked", meta: { secretId: "gh:acct", host: "github.com", login: "octocat", githubUserId: "42", version: 1, updatedAt: 7, status: "linked" } });
    const [url, opts] = f.mock.calls[0];
    expect(String(url)).toContain("/github/link/redeem");
    expect(opts.credentials).toBe("include");
    expect(JSON.parse(opts.body)).toEqual({ flow_id: "flow-1", confirm_switch: true });
  });
  it("maps 202 → pending", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 202, json: { status: "pending" } }));
    expect(await redeemGithubLink("f")).toEqual({ kind: "pending" });
  });
  it("maps 409 → identity-change with old/new logins", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 409, json: { error: "identity_change", old: "old", new: "new" } }));
    expect(await redeemGithubLink("f")).toEqual({ kind: "identity-change", oldLogin: "old", newLogin: "new" });
  });
  it("maps 404 → unknown", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 404, json: { error: "unknown_flow" } }));
    expect(await redeemGithubLink("f")).toEqual({ kind: "unknown" });
  });
  it("maps other 4xx → error with error_description", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 403, json: { error: "channel", error_description: "completer cookie required" } }));
    expect(await redeemGithubLink("f")).toEqual({ kind: "error", message: "completer cookie required" });
  });
});

describe("listGithubLinks", () => {
  it("maps snake_case rows and status", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 200, json: { links: [{ secret_id: "gh:a", host: "github.com", login: "o", github_user_id: "9", version: 2, updated_at: 5, status: "relink_required" }] } }));
    const rows = await listGithubLinks();
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ secretId: "gh:a", login: "o", version: 2, status: "relink_required" });
  });
  it("returns [] when links is absent", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 200, json: {} }));
    expect(await listGithubLinks()).toEqual([]);
  });
});

describe("revokeGithubLink", () => {
  it("POSTs form-encoded secret_id and resolves on 204", async () => {
    const f = mockFetchOnce({ status: 204 });
    vi.stubGlobal("fetch", f);
    await revokeGithubLink("gh:acct");
    const [url, opts] = f.mock.calls[0];
    expect(String(url)).toContain("/github/link/revoke");
    expect(opts.headers["Content-Type"]).toBe("application/x-www-form-urlencoded");
    expect(opts.body).toBe("secret_id=gh%3Aacct");
  });
  it("throws on non-204", async () => {
    vi.stubGlobal("fetch", mockFetchOnce({ status: 502, text: "github grant revoke failed" }));
    await expect(revokeGithubLink("gh:acct")).rejects.toThrow(/revoke failed/);
  });
});
