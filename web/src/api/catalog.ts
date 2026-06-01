import { unary } from "./connect";

export type TrustTier =
  | "TRUST_TIER_UNSPECIFIED" | "TRUST_TIER_UNVERIFIED" | "TRUST_TIER_SCANNED" | "TRUST_TIER_REVIEWED";

export interface AppSummary {
  id: string;
  displayName?: string;
  summary?: string;
  tags?: string[];
  latestVersion?: string;
  latestTier?: TrustTier;
  listed?: boolean;
}
export interface AppVersionSummary { version: string; ref?: string; tier?: TrustTier; createdAt?: string; }
export interface ManifestMount { name: string; path: string; seed?: string; }
export interface AppManifest {
  apiVersion: string; id: string; title: string; description?: string; tags?: string[];
  visibility?: string;
  agents?: { support?: string[]; exclude?: string[]; requiresAcp?: string[] };
  tools?: string[]; persona?: string; skills?: string[];
  model?: { toolUse?: boolean; minContextTokens?: number; vision?: boolean; recommendedDefault?: string };
  runtimeBaseVersion?: string;
  mounts?: ManifestMount[];
}

export async function listApps(query = ""): Promise<AppSummary[]> {
  const r = await unary<{ apps?: AppSummary[] }>("ListApps", { query });
  return r.apps ?? [];
}
export async function getApp(id: string): Promise<{ app: AppSummary; versions: AppVersionSummary[]; manifest?: AppManifest }> {
  const r = await unary<{ app: AppSummary; versions?: AppVersionSummary[]; manifest?: AppManifest }>("GetApp", { id });
  return { app: r.app, versions: r.versions ?? [], manifest: r.manifest };
}
export async function listMyApps(): Promise<AppSummary[]> {
  const r = await unary<{ apps?: AppSummary[] }>("ListMyApps", {});
  return r.apps ?? [];
}
export async function registerAppVersion(req: { manifest: AppManifest; version: string; ref: string }): Promise<{ appId: string; version: string; tier: TrustTier }> {
  return unary("RegisterAppVersion", req);
}
export async function setAppListing(appId: string, listed: boolean): Promise<void> {
  await unary<Record<string, never>>("SetAppListing", { appId, listed });
}

export function tierLabel(t?: TrustTier): { label: string; variant: "default" | "secondary" | "outline" } {
  switch (t) {
    case "TRUST_TIER_REVIEWED": return { label: "reviewed", variant: "default" };
    case "TRUST_TIER_SCANNED":  return { label: "scanned", variant: "secondary" };
    case "TRUST_TIER_UNVERIFIED": return { label: "unverified", variant: "outline" };
    default: return { label: "—", variant: "outline" };
  }
}
