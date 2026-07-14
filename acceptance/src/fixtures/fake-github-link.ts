import type { AuthStrategy } from "../auth/types";
import type { Identity } from "./identity-pool";

const MAX_DIAGNOSTIC_BODY_BYTES = 64 * 1024;
const FAKE_PROVIDER_PORT = "9099";
const FAKE_PROVIDER_AUTHORIZE_PATH = "/login/oauth/authorize";
const LINK_CALLBACK_PATH = "/github/link/callback";

export interface FakeGitHubLinkBootstrapConfig {
  asOrigin: string;
  identities: Identity[];
  auth: Pick<AuthStrategy, "cpAccessToken" | "accountId">;
  fetchImpl?: typeof fetch;
}

async function boundedBody(response: Response, secrets: string[]): Promise<string> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (size < MAX_DIAGNOSTIC_BODY_BYTES) {
      const { done, value } = await reader.read();
      if (done) break;
      const remaining = MAX_DIAGNOSTIC_BODY_BYTES - size;
      const chunk = value.subarray(0, remaining);
      chunks.push(chunk);
      size += chunk.byteLength;
      if (chunk.byteLength < value.byteLength) break;
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  let text = new TextDecoder().decode(bytes);
  for (const secret of secrets.filter(Boolean)) text = text.split(secret).join("[redacted]");
  return text;
}

async function requireStatus(
  stage: string,
  response: Response,
  expected: number,
  secrets: string[],
): Promise<void> {
  if (response.status === expected) return;
  const body = await boundedBody(response, secrets);
  throw new Error(`${stage} returned ${response.status}, expected ${expected}: ${body}`);
}

function fakeProviderAuthorizeUrl(raw: string, asOrigin: string): URL {
  let url: URL;
  let asUrl: URL;
  try {
    url = new URL(raw);
    asUrl = new URL(asOrigin);
  } catch {
    throw new Error("GitHub link start returned an invalid authorize URL");
  }
  // This helper is explicitly for the VM fake lane: its provider is served on the AS hostname at
  // port 9099. Keep the allowlist structural so an AS response cannot turn bootstrap into a fetch
  // primitive against another host or local service.
  if (
    (url.protocol !== "http:" && url.protocol !== "https:") ||
    url.hostname !== asUrl.hostname ||
    url.port !== FAKE_PROVIDER_PORT ||
    url.pathname !== FAKE_PROVIDER_AUTHORIZE_PATH ||
    url.username !== "" ||
    url.password !== "" ||
    url.hash !== ""
  ) {
    throw new Error("GitHub link start returned an unexpected authorize URL");
  }
  return url;
}

function linkCallbackUrl(raw: string, asOrigin: string): URL {
  let url: URL;
  let asUrl: URL;
  try {
    url = new URL(raw);
    asUrl = new URL(asOrigin);
  } catch {
    throw new Error("GitHub link authorize returned an invalid callback URL");
  }
  if (
    url.origin !== asUrl.origin ||
    url.pathname !== LINK_CALLBACK_PATH ||
    url.username !== "" ||
    url.password !== "" ||
    url.hash !== ""
  ) {
    throw new Error("GitHub link authorize returned an unexpected callback URL");
  }
  return url;
}

export async function bootstrapFakeGitHubLinks(
  cfg: FakeGitHubLinkBootstrapConfig,
): Promise<void> {
  const fetchImpl = cfg.fetchImpl ?? fetch;
  for (const identity of cfg.identities) {
    const bearer = await cfg.auth.cpAccessToken(identity);
    const accountId = await cfg.auth.accountId(identity);
    const secrets = [bearer, identity.token];
    const headers = {
      Authorization: `Bearer ${bearer}`,
      "Content-Type": "application/json",
    };

    const startResponse = await fetchImpl(new URL("/github/link/start", cfg.asOrigin), {
      method: "POST",
      headers,
      body: JSON.stringify({ client_kind: "device", port: 0, host: "github.com" }),
    });
    await requireStatus("GitHub link start", startResponse, 200, secrets);
    const start = await startResponse.json() as { authorize_url?: unknown; flow_id?: unknown };
    if (typeof start.authorize_url !== "string" || typeof start.flow_id !== "string" || !start.flow_id) {
      throw new Error("GitHub link start returned invalid flow metadata");
    }
    secrets.push(start.flow_id);

    const authorizeUrl = fakeProviderAuthorizeUrl(start.authorize_url, cfg.asOrigin);
    const authorizeState = authorizeUrl.searchParams.get("state");
    if (authorizeState) secrets.push(authorizeState);
    authorizeUrl.searchParams.set("login_hint", identity.token);
    const authorizeResponse = await fetchImpl(authorizeUrl, { redirect: "manual" });
    await requireStatus("GitHub link authorize", authorizeResponse, 302, secrets);
    const callbackLocation = authorizeResponse.headers.get("Location");
    if (!callbackLocation) {
      const body = await boundedBody(authorizeResponse, secrets);
      throw new Error(`GitHub link authorize returned 302 without callback Location: ${body}`);
    }
    const callbackUrl = linkCallbackUrl(callbackLocation, cfg.asOrigin);
    const callbackState = callbackUrl.searchParams.get("state");
    if (callbackState) secrets.push(callbackState);
    const callbackCode = callbackUrl.searchParams.get("code");
    if (callbackCode) secrets.push(callbackCode);

    const callbackResponse = await fetchImpl(callbackUrl, { redirect: "manual" });
    await requireStatus("GitHub link callback", callbackResponse, 200, secrets);

    const redeemResponse = await fetchImpl(new URL("/github/link/redeem", cfg.asOrigin), {
      method: "POST",
      headers,
      body: JSON.stringify({ flow_id: start.flow_id, rc: "", confirm_switch: false }),
    });
    await requireStatus("GitHub link redeem", redeemResponse, 200, secrets);
    const metadata = await redeemResponse.json() as Record<string, unknown>;
    const expected: Record<string, string> = {
      secret_id: `gh:${accountId}`,
      host: "github.com",
      login: identity.token,
      status: "linked",
    };
    for (const [field, value] of Object.entries(expected)) {
      if (metadata[field] !== value) {
        throw new Error(`GitHub link redeem returned incoherent ${field}`);
      }
    }
  }
}
