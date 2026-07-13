// Skill-bundle RPC client (sp-mwco.1.9 §4.7): ListBundles/GetBundle for the catalog's Bundles
// section, ReingestBundle + GetBundleDiff + RepinProfileBundle for the "Check for updates"
// diff-gated re-pin flow (§4.9 — the diff IS the supply-chain gate, never bypassed client-side).
import { unary } from "./connect";

// --- int64/uint64-as-string coercion -----------------------------------------
// Proto-JSON encodes int64/uint64 fields as strings; seq counters are compared/rendered as
// numbers throughout the UI (update-available badges, "vN" labels), so they're coerced here
// at the API boundary rather than leaking wire-format strings into components.
function toNum(v: string | number | undefined): number {
  if (v === undefined) return 0;
  return typeof v === "number" ? v : Number(v);
}

// --- Types ---------------------------------------------------------------------

export interface BundleSummary {
  bundleId: string;
  name: string;
  sourceUrl?: string;
  sourceRef?: string;
  sourceSubdir?: string;
  createdAt?: string;
  updatedAt?: string;
  latestVersionId?: string;
  latestSeq: number;
  memberCount: number;
}

export interface BundleVersion {
  versionId: string;
  seq: number;
  sourceCommit?: string;
  createdAt?: string;
}

export interface BundleMember {
  catalogId: string;
  sourceSubdir: string;
  name: string;
  description?: string;
  sha256?: string;
  position: number;
}

export interface GetBundleResult {
  bundle: BundleSummary;
  versions: BundleVersion[];
  members: BundleMember[];
}

export interface ReingestBundleResult {
  versionId: string;
  changed: boolean;
  addedSubdirs: string[];
  updatedSubdirs: string[];
  removedSubdirs: string[];
  diffToken: string;
  fromVersionId: string;
  notModified: boolean;
  warnings: string[];
}

export type MemberDiffChange = "CHANGE_UNSPECIFIED" | "ADDED" | "REMOVED" | "CHANGED";

export interface MemberDiff {
  sourceSubdir: string;
  change: MemberDiffChange;
  name: string;
  oldSha256?: string;
  newSha256?: string;
  oldSkillMd?: string;
  newSkillMd?: string;
  oldTruncated?: boolean;
  newTruncated?: boolean;
  bodyUnavailable?: boolean;
}

export interface GetBundleDiffResult {
  members: MemberDiff[];
  fromCommit: string;
  toCommit: string;
  diffToken: string;
}

export interface RepinProfileBundleResult {
  version: number;
  warnings: string[];
}

// --- Bundle map/list/diff/re-pin API --------------------------------------------

function mapBundleSummary(b: Partial<BundleSummary> & { bundleId: string; name: string }): BundleSummary {
  return { ...b, latestSeq: toNum(b.latestSeq), memberCount: toNum(b.memberCount) };
}

export async function listBundles(): Promise<BundleSummary[]> {
  const r = await unary<{ bundles?: (Partial<BundleSummary> & { bundleId: string; name: string })[] }>(
    "ListBundles", {},
  );
  return (r.bundles ?? []).map(mapBundleSummary);
}

export async function getBundle(bundleId: string): Promise<GetBundleResult> {
  const r = await unary<{
    bundle: Partial<BundleSummary> & { bundleId: string; name: string };
    versions?: (Partial<BundleVersion> & { versionId: string })[];
    members?: BundleMember[];
  }>("GetBundle", { bundleId });
  return {
    bundle: mapBundleSummary(r.bundle),
    versions: (r.versions ?? []).map((v) => ({ ...v, seq: toNum(v.seq) })),
    members: r.members ?? [],
  };
}

export async function reingestBundle(bundleId: string): Promise<ReingestBundleResult> {
  const r = await unary<Partial<ReingestBundleResult> & { versionId: string }>("ReingestBundle", { bundleId });
  return {
    versionId: r.versionId,
    changed: r.changed ?? false,
    addedSubdirs: r.addedSubdirs ?? [],
    updatedSubdirs: r.updatedSubdirs ?? [],
    removedSubdirs: r.removedSubdirs ?? [],
    diffToken: r.diffToken ?? "",
    fromVersionId: r.fromVersionId ?? "",
    notModified: r.notModified ?? false,
    warnings: r.warnings ?? [],
  };
}

export async function getBundleDiff(
  bundleId: string,
  fromVersion: string,
  toVersion: string,
): Promise<GetBundleDiffResult> {
  const r = await unary<Partial<GetBundleDiffResult>>("GetBundleDiff", { bundleId, fromVersion, toVersion });
  return {
    members: r.members ?? [],
    fromCommit: r.fromCommit ?? "",
    toCommit: r.toCommit ?? "",
    diffToken: r.diffToken ?? "",
  };
}

export async function repinProfileBundle(
  profileId: string,
  entryId: string,
  versionId: string,
  diffToken: string,
  expectedVersion: number,
): Promise<RepinProfileBundleResult> {
  const r = await unary<{ version: string | number; warnings?: string[] }>("RepinProfileBundle", {
    profileId,
    entryId,
    versionId,
    diffToken,
    expectedVersion,
  });
  return { version: toNum(r.version), warnings: r.warnings ?? [] };
}
