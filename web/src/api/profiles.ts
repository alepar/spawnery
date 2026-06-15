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
  | "PROFILE_ENTRY_SOURCE_CUSTOM";

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

export async function getProfile(profileId: string): Promise<Profile> {
  const r = await unary<{ profile?: Partial<Profile> & { profileId: string; name: string; version: number } }>(
    "GetProfile", { profileId },
  );
  const p = r.profile!;
  // Decode proto `bytes` field customInline from base64 (Connect-JSON encoding) to raw string.
  const entries = (p.entries ?? []).map((e) =>
    e.customInline !== undefined
      ? { ...e, customInline: base64ToUtf8(e.customInline) }
      : e,
  );
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
): Promise<{ entryId: string; version: number }> {
  // proto `bytes` field customInline must be base64-encoded in Connect-JSON.
  const wireEntry = entry.customInline !== undefined
    ? { ...entry, customInline: utf8ToBase64(entry.customInline) }
    : entry;
  return unary<{ entryId: string; version: number }>("AddProfileEntry", {
    profileId,
    expectedVersion,
    entry: wireEntry,
  });
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
