/**
 * AS HTTP client for the owner-facing GitHub link flow (spec r2 §6.1/§6.2).
 *
 * Plain fetch against the AS origin (like auth/refresh.ts). The flow surfaces NO token material:
 * redeem/list return metadata only. The redemption secret is the HttpOnly completer cookie the
 * browser auto-attaches to /github/link/redeem under credentials:'include' (corsCredentialed, S1).
 */
import { asHttpUrl } from "@/config/endpoints";
import { getAccessToken } from "@/auth/session";

export type GithubLinkStatus = "linked" | "revoked" | "relink_required";

export interface GithubLinkMeta {
  secretId: string;
  host: string;
  login: string;
  githubUserId: string;
  version: number;
  updatedAt: number;
  status: GithubLinkStatus;
}

export interface StartResult {
  authorizeUrl: string;
  flowId: string;
}

export type RedeemResult =
  | { kind: "linked"; meta: GithubLinkMeta }
  | { kind: "pending" }
  | { kind: "identity-change"; oldLogin: string; newLogin: string }
  | { kind: "unknown" }
  | { kind: "error"; message: string };

interface RawMeta {
  secret_id?: string;
  host?: string;
  login?: string;
  github_user_id?: string;
  version?: number;
  updated_at?: number;
  status?: string;
}

function authHeaders(): Record<string, string> {
  return { Authorization: `Bearer ${getAccessToken()}` };
}

function parseMeta(raw: RawMeta): GithubLinkMeta {
  const status: GithubLinkStatus =
    raw.status === "revoked" || raw.status === "relink_required" ? raw.status : "linked";
  return {
    secretId: raw.secret_id ?? "",
    host: raw.host ?? "",
    login: raw.login ?? "",
    githubUserId: raw.github_user_id ?? "",
    version: raw.version ?? 0,
    updatedAt: raw.updated_at ?? 0,
    status,
  };
}

async function errorMessage(res: Response): Promise<string> {
  const text = await res.text().catch(() => "");
  try {
    const j = JSON.parse(text) as { error_description?: string; error?: string };
    return j.error_description || j.error || String(res.status);
  } catch {
    return text || String(res.status);
  }
}

export async function startGithubLink(opts: { host?: string } = {}): Promise<StartResult> {
  const res = await fetch(asHttpUrl("/github/link/start"), {
    method: "POST",
    headers: { ...authHeaders(), "Content-Type": "application/json" },
    body: JSON.stringify({ client_kind: "web", ...(opts.host ? { host: opts.host } : {}) }),
  });
  if (!res.ok) throw new Error(`github link start failed: ${await errorMessage(res)}`);
  const json = (await res.json()) as { authorize_url?: string; flow_id?: string };
  if (!json.authorize_url || !json.flow_id) throw new Error("github link start: malformed response");
  return { authorizeUrl: json.authorize_url, flowId: json.flow_id };
}

export async function redeemGithubLink(flowId: string, confirmSwitch = false): Promise<RedeemResult> {
  let res: Response;
  try {
    res = await fetch(asHttpUrl("/github/link/redeem"), {
      method: "POST",
      credentials: "include", // HttpOnly completer cookie rides cross-origin (corsCredentialed, S1)
      headers: { ...authHeaders(), "Content-Type": "application/json" },
      body: JSON.stringify({ flow_id: flowId, confirm_switch: confirmSwitch }),
    });
  } catch (e) {
    return { kind: "error", message: String(e) };
  }
  if (res.status === 200) return { kind: "linked", meta: parseMeta((await res.json()) as RawMeta) };
  if (res.status === 202) return { kind: "pending" };
  if (res.status === 409) {
    const body = (await res.json().catch(() => ({}))) as { old?: string; new?: string };
    return { kind: "identity-change", oldLogin: body.old ?? "", newLogin: body.new ?? "" };
  }
  if (res.status === 404) return { kind: "unknown" };
  return { kind: "error", message: await errorMessage(res) };
}

export async function listGithubLinks(): Promise<GithubLinkMeta[]> {
  const res = await fetch(asHttpUrl("/github/links"), { method: "GET", headers: authHeaders() });
  if (!res.ok) throw new Error(`github links list failed: ${await errorMessage(res)}`);
  const json = (await res.json()) as { links?: RawMeta[] };
  return (json.links ?? []).map(parseMeta);
}

export async function revokeGithubLink(secretId: string): Promise<void> {
  const res = await fetch(asHttpUrl("/github/link/revoke"), {
    method: "POST",
    headers: { ...authHeaders(), "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ secret_id: secretId }).toString(),
  });
  if (res.status === 204) return;
  throw new Error(`github link revoke failed: ${await errorMessage(res)}`);
}
