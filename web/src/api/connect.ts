// Calls the CP's ConnectRPC unary methods via plain fetch (Connect JSON, camelCase fields).
import { cpHttpUrl } from "@/config/endpoints";

export const DEV_TOKEN = "dev-token";

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
