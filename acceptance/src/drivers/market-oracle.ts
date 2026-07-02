/**
 * marketOracle: a Connect-JSON client for the marketplace RPCs (ListApps/GetApp/ListMyApps/
 * RegisterAppVersion/SetAppListing on cp.v1.SpawnService), mirroring api.ts's wire shape exactly
 * (plain fetch, no codegen). Cross-checks surface-driven writes (design §Assertion strategy); not
 * the sole source of truth. Kept separate from ApiDriver to stay disjoint from Phase-1 edits to
 * api.ts (see the sp-tq0t.7 plan's isolation note) — the small duplication is the isolation cost.
 */

export type TrustTier =
  | "TRUST_TIER_UNSPECIFIED"
  | "TRUST_TIER_UNVERIFIED"
  | "TRUST_TIER_SCANNED"
  | "TRUST_TIER_REVIEWED";

export interface AppRef {
  id: string;
  displayName: string;
  latestVersion: string;
  latestTier?: TrustTier;
  listed: boolean;
}

export interface AppVersionRef {
  version: string;
  ref?: string;
  tier?: TrustTier;
  createdAt?: string;
}

export interface ManifestMountRef {
  name: string;
  path: string;
  seed?: string;
  durability?: string;
  github?: boolean;
}

export interface AppManifestRef {
  apiVersion: string;
  id: string;
  title: string;
  description?: string;
  tags?: string[];
  visibility?: string;
  mounts?: ManifestMountRef[];
}

export interface GetAppResult {
  app: AppRef;
  versions: AppVersionRef[];
  manifest?: AppManifestRef;
}

export interface RegisterAppVersionSpec {
  manifest: AppManifestRef;
  version: string;
  ref: string;
}

export interface RegisterAppVersionResult {
  appId: string;
  version: string;
  tier: TrustTier;
}

function toAppRef(a: Record<string, unknown>): AppRef {
  return {
    id: a.id as string,
    displayName: (a.displayName as string) ?? "",
    latestVersion: (a.latestVersion as string) ?? "",
    latestTier: a.latestTier as TrustTier | undefined,
    listed: (a.listed as boolean) ?? false,
  };
}

export class MarketOracle {
  constructor(
    private readonly cpEndpoint: string,
    private readonly token: string,
  ) {}

  private async call<T>(method: string, body: unknown): Promise<T> {
    const res = await fetch(`${this.cpEndpoint}/cp.v1.SpawnService/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Connect-Protocol-Version": "1",
        Authorization: `Bearer ${this.token}`,
      },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`${method} failed: ${res.status} ${text}`);
    }
    return (await res.json()) as T;
  }

  async listApps(query = ""): Promise<AppRef[]> {
    const resp = await this.call<{ apps?: Record<string, unknown>[] }>("ListApps", { query });
    return (resp.apps ?? []).map(toAppRef);
  }

  async getApp(id: string): Promise<GetAppResult> {
    const resp = await this.call<{
      app: Record<string, unknown>;
      versions?: AppVersionRef[];
      manifest?: AppManifestRef;
    }>("GetApp", { id });
    return { app: toAppRef(resp.app), versions: resp.versions ?? [], manifest: resp.manifest };
  }

  async listMyApps(): Promise<AppRef[]> {
    const resp = await this.call<{ apps?: Record<string, unknown>[] }>("ListMyApps", {});
    return (resp.apps ?? []).map(toAppRef);
  }

  async registerAppVersion(spec: RegisterAppVersionSpec): Promise<RegisterAppVersionResult> {
    return this.call<RegisterAppVersionResult>("RegisterAppVersion", spec);
  }

  async setAppListing(appId: string, listed: boolean): Promise<void> {
    await this.call<Record<string, never>>("SetAppListing", { appId, listed });
  }
}
