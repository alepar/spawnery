import type { AuthStrategy } from "../auth/types";
import type { Identity } from "./identity-pool";

const MAX_DIAGNOSTIC_BODY_BYTES = 64 * 1024;

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

    const authorizeUrl = new URL(start.authorize_url);
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
    const callbackUrl = new URL(callbackLocation, authorizeUrl);
    const callbackState = callbackUrl.searchParams.get("state");
    if (callbackState) secrets.push(callbackState);

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
