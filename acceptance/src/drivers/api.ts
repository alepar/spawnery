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

// --- Customization: profiles, catalog entries, secrets (sp-tq0t.8) ---
// Field names are the Connect-JSON camelCase of proto/cp/v1 (Profile/ProfileSummary/ProfileEntry,
// CustomizationCatalogEntry/CatalogEntrySummary, Secret/SecretSummary). Enums serialize as strings
// (e.g. "PROFILE_ENTRY_KIND_SKILL", "USER_SECRET_TYPE_GENERIC_KV", "ARTIFACT_TARGET_AGENT").

export interface ProfileSummary {
  profileId: string;
  name: string;
  version: number;
  updatedAt: string;
}

export interface ProfileEntry {
  entryId: string;
  kind: string;
  name: string;
  source: string;
  catalogId?: string;
  customInline?: string; // base64 (proto bytes)
  targets?: string[];
  mcpSecretRefs?: string[];
}

export interface Profile {
  profileId: string;
  name: string;
  version: number;
  updatedAt: string;
  // Connect-JSON omits empty repeated fields entirely — normalized to [] by getProfile below so
  // scenario assertions never have to guard against undefined.
  entries: ProfileEntry[];
  secretIds: string[];
}

export interface CatalogEntrySummary {
  catalogId: string;
  kind: string;
  name: string;
  description: string;
}

export interface CatalogEntry {
  catalogId: string;
  creatorId: string;
  kind: string;
  name: string;
  description: string;
  content?: string; // base64 (proto bytes)
  // Connect-JSON omits a false bool — normalized by getCatalogEntry below.
  listed: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface SecretSummary {
  secretId: string;
  type: string;
  name: string;
  provider?: string;
  targetContainer: string;
  envVarName?: string;
  destPath?: string;
  version: number;
  devicesetEpoch: number;
  updatedAt: string;
}

export interface SecretDetail extends SecretSummary {
  envelope?: string; // base64 (proto bytes)
  createdAt: string;
}

export interface SecretWrite {
  secretId: string;
  type: string;
  name: string;
  provider?: string;
  targetContainer: string;
  envVarName?: string;
  destPath?: string;
  devicesetEpoch?: number;
  envelope: string; // base64 (proto bytes)
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

  // --- Profiles (oracle reads; cross-check for the cli-primary ProfileCli) ---

  async listProfiles(): Promise<ProfileSummary[]> {
    const resp = await this.call<{ profiles?: ProfileSummary[] }>("ListProfiles", {});
    return resp.profiles ?? [];
  }

  async getProfile(profileId: string): Promise<Profile> {
    const resp = await this.call<{ profile: Profile }>("GetProfile", { profileId });
    return { ...resp.profile, entries: resp.profile.entries ?? [], secretIds: resp.profile.secretIds ?? [] };
  }

  // --- Catalog entries (oracle reads; cross-check for the cli-primary CatalogCli) ---

  async listCatalogEntries(): Promise<CatalogEntrySummary[]> {
    const resp = await this.call<{ entries?: CatalogEntrySummary[] }>("ListCatalogEntries", {});
    return resp.entries ?? [];
  }

  async getCatalogEntry(catalogId: string): Promise<CatalogEntry> {
    const resp = await this.call<{ entry: CatalogEntry }>("GetCatalogEntry", { catalogId });
    return { ...resp.entry, listed: resp.entry.listed ?? false };
  }

  // --- Secrets (oracle-only: spawnctl has no `secret` subcommand — CLI parity gap, sp-tq0t) ---

  async createSecret(write: SecretWrite): Promise<SecretDetail> {
    const resp = await this.call<{ secret: SecretDetail }>("CreateSecret", { secret: write });
    return resp.secret;
  }

  async listSecrets(): Promise<SecretSummary[]> {
    const resp = await this.call<{ secrets?: SecretSummary[] }>("ListSecrets", {});
    return resp.secrets ?? [];
  }

  async getSecret(secretId: string): Promise<SecretDetail> {
    const resp = await this.call<{ secret: SecretDetail }>("GetSecret", { secretId });
    return resp.secret;
  }

  async deleteSecret(secretId: string): Promise<void> {
    await this.call("DeleteSecret", { secretId });
  }
}
