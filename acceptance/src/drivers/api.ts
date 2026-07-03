/**
 * apiDriver: a Connect-JSON client following web/src/api/connect.ts's wire shape exactly (plain
 * fetch, no @connectrpc/@bufbuild codegen — web/ has no such dep either). This is the
 * surface-agnostic cross-check oracle, NOT the sole source of truth (design §API oracle). It
 * reads spawn status via ListSpawns — there is no GetSpawn RPC.
 *
 * The token may be a static string (DevTokenAuth) or an async provider (OAuthPoPAuth,
 * auth/oauthpop.ts) that is called fresh before every request — a run can span longer than the
 * AS's 15-minute access-token TTL (design §NFRs' cost/wall-clock cap defaults to 30 min), so a
 * token captured once at driver-construction time would go stale mid-run.
 */

import type { SpawnStatus } from "./types";

export interface SpawnSummary {
  spawnId: string;
  appId: string;
  appVersion: string;
  model: string;
  status: SpawnStatus;
  createdAt: string; // int64 serializes as a JSON string in Connect-JSON
  lastUsedAt: string;
  name: string;
  modelApplied: boolean;
  parentSpawnId?: string;
}

export interface AppSummary {
  id: string;
  displayName: string;
  latestVersion: string;
  listed: boolean;
}

export interface CreateSpawnApiRequest {
  appId: string;
  model?: string;
  name?: string;
  version?: string;
  profileId?: string;
}

function stripStatusPrefix(wire: string): SpawnStatus {
  return wire.replace(/^SPAWN_STATUS_/, "") as SpawnStatus;
}

/** TokenSource is a static bearer (dev-token) or an async provider called fresh per request
 * (OAuth-PoP, whose token expires and must be proactively refreshed — see auth/oauthpop.ts). */
export type TokenSource = string | (() => Promise<string>);

export class ApiDriver {
  constructor(
    private readonly cpEndpoint: string,
    private readonly tokenSource: TokenSource,
  ) {}

  private resolveToken(): Promise<string> {
    return typeof this.tokenSource === "string" ? Promise.resolve(this.tokenSource) : this.tokenSource();
  }

  private async call<T>(method: string, body: unknown): Promise<T> {
    const token = await this.resolveToken();
    const res = await fetch(`${this.cpEndpoint}/cp.v1.SpawnService/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Connect-Protocol-Version": "1",
        Authorization: `Bearer ${token}`,
      },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`${method} failed: ${res.status} ${text}`);
    }
    return (await res.json()) as T;
  }

  async listSpawns(): Promise<SpawnSummary[]> {
    const resp = await this.call<{ spawns?: Record<string, unknown>[] }>("ListSpawns", {});
    return (resp.spawns ?? []).map((s) => ({
      spawnId: s.spawnId as string,
      appId: s.appId as string,
      appVersion: (s.appVersion as string) ?? "",
      model: (s.model as string) ?? "",
      status: stripStatusPrefix((s.status as string) ?? "SPAWN_STATUS_UNSPECIFIED"),
      createdAt: (s.createdAt as string) ?? "0",
      lastUsedAt: (s.lastUsedAt as string) ?? "0",
      name: (s.name as string) ?? "",
      modelApplied: (s.modelApplied as boolean) ?? false,
      parentSpawnId: s.parentSpawnId as string | undefined,
    }));
  }

  /** findSpawn reads back a single spawn's status via ListSpawns + filter (no GetSpawn RPC exists). */
  async findSpawn(spawnId: string): Promise<SpawnSummary | undefined> {
    const spawns = await this.listSpawns();
    return spawns.find((s) => s.spawnId === spawnId);
  }

  async listApps(query = ""): Promise<AppSummary[]> {
    const resp = await this.call<{ apps?: Record<string, unknown>[] }>("ListApps", { query });
    return (resp.apps ?? []).map((a) => ({
      id: a.id as string,
      displayName: (a.displayName as string) ?? "",
      latestVersion: (a.latestVersion as string) ?? "",
      listed: (a.listed as boolean) ?? false,
    }));
  }

  async createSpawn(req: CreateSpawnApiRequest): Promise<string> {
    const resp = await this.call<{ spawnId: string }>("CreateSpawn", req);
    return resp.spawnId;
  }

  async deleteSpawn(spawnId: string, destroyData = true): Promise<void> {
    await this.call("DeleteSpawn", { spawnId, destroyData });
  }

  async stopSpawn(spawnId: string): Promise<void> {
    await this.call("StopSpawn", { spawnId });
  }
}
