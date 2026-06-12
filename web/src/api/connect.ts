// Calls the CP's ConnectRPC unary methods via plain fetch (Connect JSON, camelCase fields).
import { cpHttpUrl } from "@/config/endpoints";

// Token is baked in at build time from VITE_AUTH_TOKEN. In development (.env.development)
// this is "dev-token"; in release builds it comes from a GitHub secret so the literal
// "dev-token" string never appears in the signed production bundle (pre-sign scan rejects it).
export const DEV_TOKEN = import.meta.env.VITE_AUTH_TOKEN ?? "";

export async function unary<T>(method: string, body: unknown): Promise<T> {
  const res = await fetch(cpHttpUrl(`/cp.v1.SpawnService/${method}`), {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      Authorization: `Bearer ${DEV_TOKEN}`,
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}
