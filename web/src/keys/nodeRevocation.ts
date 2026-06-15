import { asHttpUrl } from "@/config/endpoints";

import type { RevocationChecker } from "./subkey";

type Fetcher = typeof fetch;

interface Options {
  fetcher?: Fetcher;
  ttlMs?: number;
  nowMs?: () => number;
  url?: string;
}

export function asNodeRevocationChecker(opts: Options = {}): RevocationChecker {
  const fetcher = opts.fetcher ?? fetch;
  const ttlMs = opts.ttlMs ?? 0;
  const nowMs = opts.nowMs ?? (() => Date.now());
  const url = opts.url ?? asHttpUrl("/node-revocations");
  let expiresAt = 0;
  let cached: Set<string> | null = null;
  let inFlight: Promise<Set<string>> | null = null;

  async function load(): Promise<Set<string>> {
    if (ttlMs > 0 && cached && nowMs() < expiresAt) return cached;
    if (!inFlight) {
      inFlight = fetcher(url, {
        method: "GET",
        cache: "no-store",
        headers: { Accept: "application/json" },
      }).then(async (res) => {
        if (!res.ok) throw new Error(`revocation list unavailable: AS returned ${res.status}`);
        const body = await res.json() as { revoked_node_ids?: unknown };
        if (!Array.isArray(body.revoked_node_ids) || !body.revoked_node_ids.every((v) => typeof v === "string")) {
          throw new Error("revocation list unavailable: malformed AS response");
        }
        const set = new Set(body.revoked_node_ids);
        if (ttlMs > 0) {
          cached = set;
          expiresAt = nowMs() + ttlMs;
        }
        return set;
      }).finally(() => {
        inFlight = null;
      });
    }
    return inFlight;
  }

  return {
    async check(nodeId: string): Promise<void> {
      const revoked = await load();
      if (revoked.has(nodeId)) {
        throw new Error("node is on the AS revocation deny-list");
      }
    },
  };
}
