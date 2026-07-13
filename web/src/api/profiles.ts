import { unary } from "./connect";

// --- Base64 helpers for proto `bytes` fields (Connect-JSON encodes bytes as base64) ---

/** Encode a raw UTF-8 string to standard base64 for a proto `bytes` wire field. */
function utf8ToBase64(str: string): string {
  const bytes = new TextEncoder().encode(str);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary);
}

/** Decode a base64 proto `bytes` field back to a raw UTF-8 string. */
function base64ToUtf8(b64: string): string {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

// --- Types & enums -----------------------------------------------------------

export type ProfileEntryKind =
  | "PROFILE_ENTRY_KIND_SKILL"
  | "PROFILE_ENTRY_KIND_MCP"
  | "PROFILE_ENTRY_KIND_CONFIG"
  | "PROFILE_ENTRY_KIND_PLUGIN";

export type ProfileEntrySource =
  | "PROFILE_ENTRY_SOURCE_CATALOG_REF"
  | "PROFILE_ENTRY_SOURCE_CUSTOM"
  | "PROFILE_ENTRY_SOURCE_BUNDLE_REF";

export interface ProfileSummary {
  profileId: string;
  name: string;
  version: number;
  updatedAt?: string;
}

export interface ProfileEntry {
  entryId: string;
  kind: ProfileEntryKind;
  name: string;
  source: ProfileEntrySource;
  catalogId?: string;
  customInline?: string;
  targets?: string[];
  mcpSecretRefs?: string[];
  // bundle_ref fields (sp-mwco.1.8 §4.4): set when source=BUNDLE_REF. bundleId/versionId pin
  // this entry to one exact bundle version; excludedSubdirs/memberRenames are per-member
  // overrides keyed by the bundle member's source_subdir. pinnedSeq/latestSeq/memberCount are
  // read-only, server-populated by GetProfile — "update available" is latestSeq > pinnedSeq,
  // computed client-side (there is no such wire field).
  bundleId?: string;
  versionId?: string;
  excludedSubdirs?: string[];
  memberRenames?: Record<string, string>;
  pinnedSeq?: number;
  latestSeq?: number;
  memberCount?: number;
}

export interface Profile {
  profileId: string;
  name: string;
  version: number;
  updatedAt?: string;
  entries: ProfileEntry[];
  secretIds?: string[];
}

export interface CatalogEntrySummary {
  catalogId: string;
  kind: ProfileEntryKind;
  name: string;
  description?: string;
  // Provenance (sp-mwco.3.1). Empty/absent for inline (non-URL) entries.
  sourceUrl?: string;
  sourceRef?: string;
  sourceSubdir?: string;
  sourceCommit?: string;
  sha256?: string;
  size?: string; // proto-JSON encodes int64 as a string
  bundleMember?: boolean;
}

export interface CustomizationCatalogEntry {
  catalogId: string;
  creatorId?: string;
  kind: ProfileEntryKind;
  name: string;
  description?: string;
  content?: string;
  listed?: boolean;
  createdAt?: string;
  updatedAt?: string;
  // Provenance (sp-mwco.3.1). Empty/absent for inline (non-URL) entries.
  sourceUrl?: string;
  sourceRef?: string;
  sourceSubdir?: string;
  sourceCommit?: string;
  sha256?: string;
  size?: string; // proto-JSON encodes int64 as a string
  bundleMember?: boolean;
}

// --- Display helpers ---------------------------------------------------------

export const KIND_LABEL: Record<ProfileEntryKind, string> = {
  PROFILE_ENTRY_KIND_SKILL: "Skill",
  PROFILE_ENTRY_KIND_MCP: "MCP",
  PROFILE_ENTRY_KIND_CONFIG: "Config",
  PROFILE_ENTRY_KIND_PLUGIN: "Plugin",
};

/** Maps a ProfileEntryKind enum to the capability kind string used in capabilities.ts. */
export function kindToCapKind(kind: ProfileEntryKind): "skill" | "mcp" | "config" | "plugin" {
  switch (kind) {
    case "PROFILE_ENTRY_KIND_SKILL":  return "skill";
    case "PROFILE_ENTRY_KIND_MCP":    return "mcp";
    case "PROFILE_ENTRY_KIND_CONFIG": return "config";
    case "PROFILE_ENTRY_KIND_PLUGIN": return "plugin";
  }
}

// --- Profile CRUD API --------------------------------------------------------

export async function listProfiles(): Promise<ProfileSummary[]> {
  const r = await unary<{ profiles?: ProfileSummary[] }>("ListProfiles", {});
  return r.profiles ?? [];
}

export async function createProfile(name: string): Promise<{ profileId: string; version: number }> {
  return unary<{ profileId: string; version: number }>("CreateProfile", { name });
}

/** Wire shape of a ProfileEntry before decoding: pinnedSeq/latestSeq are proto int64, so they
 * arrive as strings (Connect-JSON), not the `number` the app-facing ProfileEntry type declares. */
type WireProfileEntry = Omit<ProfileEntry, "pinnedSeq" | "latestSeq"> & {
  pinnedSeq?: string | number;
  latestSeq?: string | number;
};

export async function getProfile(profileId: string): Promise<Profile> {
  const r = await unary<{
    profile?: Omit<Partial<Profile>, "entries"> & {
      profileId: string; name: string; version: number; entries?: WireProfileEntry[];
    };
  }>("GetProfile", { profileId });
  const p = r.profile!;
  // Decode proto `bytes` field customInline from base64 (Connect-JSON encoding) to raw string,
  // and int64 pinnedSeq/latestSeq from wire strings to numbers.
  const entries: ProfileEntry[] = (p.entries ?? []).map((e) => ({
    ...e,
    customInline: e.customInline !== undefined ? base64ToUtf8(e.customInline) : e.customInline,
    pinnedSeq: e.pinnedSeq !== undefined ? Number(e.pinnedSeq) : undefined,
    latestSeq: e.latestSeq !== undefined ? Number(e.latestSeq) : undefined,
  }));
  return { ...p, entries, secretIds: p.secretIds ?? [] };
}

export async function updateProfile(
  profileId: string,
  expectedVersion: number,
  name: string,
): Promise<{ version: number }> {
  return unary<{ version: number }>("UpdateProfile", { profileId, expectedVersion, name });
}

export async function deleteProfile(profileId: string): Promise<void> {
  await unary<Record<string, never>>("DeleteProfile", { profileId });
}

export async function addProfileEntry(
  profileId: string,
  expectedVersion: number,
  entry: Omit<ProfileEntry, "entryId">,
): Promise<{ entryId: string; version: number; warnings: string[] }> {
  // proto `bytes` field customInline must be base64-encoded in Connect-JSON.
  const wireEntry = entry.customInline !== undefined
    ? { ...entry, customInline: utf8ToBase64(entry.customInline) }
    : entry;
  const r = await unary<{ entryId: string; version: number; warnings?: string[] }>("AddProfileEntry", {
    profileId,
    expectedVersion,
    entry: wireEntry,
  });
  return { ...r, warnings: r.warnings ?? [] };
}

export async function removeProfileEntry(
  profileId: string,
  expectedVersion: number,
  entryId: string,
): Promise<{ version: number }> {
  return unary<{ version: number }>("RemoveProfileEntry", { profileId, expectedVersion, entryId });
}

// --- Customization Catalog API -----------------------------------------------

export async function listCatalogEntries(): Promise<CatalogEntrySummary[]> {
  const r = await unary<{ entries?: CatalogEntrySummary[] }>("ListCatalogEntries", {});
  return r.entries ?? [];
}

export async function getCatalogEntry(catalogId: string): Promise<CustomizationCatalogEntry> {
  const r = await unary<{ entry: CustomizationCatalogEntry }>("GetCatalogEntry", { catalogId });
  return r.entry;
}

/**
 * Delete a catalog entry. A referenced entry (a profile's catalog_ref, or a bundle-version
 * membership) is rejected with FailedPrecondition — the message carries counts only, never
 * profile/owner ids (sp-mwco.3.3 §4.3). Pass `force: true` to delete anyway; force does NOT
 * override a bundle-version membership (delete the bundle version instead).
 */
export async function deleteCatalogEntry(catalogId: string, opts?: { force?: boolean }): Promise<void> {
  await unary<Record<string, never>>("DeleteCatalogEntry", { catalogId, force: opts?.force ?? false });
}

/**
 * Delete a skill bundle (creator-only). Cascades to its versions and their members, but not to
 * the member catalog rows themselves (content-identity dedup — they may be shared elsewhere).
 * Rejected with FailedPrecondition (counts-only message) if any profile references the bundle,
 * unless `force: true`.
 */
export async function deleteBundle(bundleId: string, opts?: { force?: boolean }): Promise<void> {
  await unary<Record<string, never>>("DeleteBundle", { bundleId, force: opts?.force ?? false });
}

/**
 * Delete one version of a skill bundle (creator-only). Sibling versions are untouched. Rejected
 * with FailedPrecondition (counts-only message) if any profile is pinned to this version, unless
 * `force: true`.
 */
export async function deleteBundleVersion(versionId: string, opts?: { force?: boolean }): Promise<void> {
  await unary<Record<string, never>>("DeleteBundleVersion", { versionId, force: opts?.force ?? false });
}

// --- Skill URL ingest API -----------------------------------------------------

export interface IngestSkillFromURLInput {
  url: string;
  ref?: string;
  subdir?: string;
  name?: string;
  description?: string;
}

// IngestSkillFromURLResult.catalogId keeps its historical meaning: the first member (the
// bundle-of-one case, by far the common one, is unchanged). bundleId/versionId/memberCatalogIds
// let a caller resolve the full bundle for a multi-skill repo (sp-mwco.1.7/1.9).
export interface IngestSkillFromURLResult {
  catalogId: string;
  bundleId: string;
  versionId: string;
  memberCatalogIds: string[];
  skippedEntries: string[]; // symlinks/devices skipped, never fetched
  warnings: string[];       // nested-skill warnings, cap hints
  changed: boolean;         // false => a re-paste that cut no new version
}

/**
 * Ingest a skill (or multi-skill bundle) from a GitHub URL into the catalog.
 * Optional fields are omitted when empty to match proto "unset" semantics.
 */
export async function ingestSkillFromURL(input: IngestSkillFromURLInput): Promise<IngestSkillFromURLResult> {
  const body: Record<string, string> = { url: input.url };
  if (input.ref)         body.ref         = input.ref;
  if (input.subdir)      body.subdir      = input.subdir;
  if (input.name)        body.name        = input.name;
  if (input.description) body.description = input.description;
  const r = await unary<Partial<IngestSkillFromURLResult> & { catalogId: string }>("IngestSkillFromURL", body);
  return {
    catalogId: r.catalogId,
    bundleId: r.bundleId ?? "",
    versionId: r.versionId ?? "",
    memberCatalogIds: r.memberCatalogIds ?? [],
    skippedEntries: r.skippedEntries ?? [],
    warnings: r.warnings ?? [],
    changed: r.changed ?? false,
  };
}

/**
 * Extract the actionable server message from a Connect-JSON error thrown by `unary`.
 *
 * `unary` throws `Error("<method> failed: <status> <bodyText>")` where bodyText is
 * `{"code":"...","message":"..."}`. We parse the trailing JSON and return `.message`.
 * Falls back to the raw Error.message when the body is not valid JSON.
 */
export function connectErrorMessage(e: unknown): string {
  const raw = e instanceof Error ? e.message : String(e);
  // Find the last '{...}' in the string — that's the Connect-JSON error body.
  const match = raw.match(/(\{.*\})\s*$/);
  if (match) {
    try {
      const parsed = JSON.parse(match[1]) as { message?: string };
      if (parsed.message) return parsed.message;
    } catch {
      // not valid JSON — fall through
    }
  }
  return raw;
}
