/**
 * AcceptanceClient: the acceptance suite's oracle/janitor/setup client, wrapping @spawnery/client's
 * SpawnClient so createSpawn (and every other lifecycle op) SIGNS the A4 intent — the fix for the
 * enforced-node blocker the old hand-rolled `ApiDriver` (no signing at all) could never clear
 * (design doc 2026-07-06-client-sdk-signing-design.md). It is an oracle/helper, not a driver under
 * test: there is no `· api` test arm, only `web`/`cli`.
 *
 * Strategy: preserve every acceptance-facing DTO shape from the old drivers/api.ts (SpawnSummary,
 * AppSummary, CreateSpawnApiRequest, Profile*, CatalogEntry*, Secret*) — including their Connect-JSON
 * conventions (int64 as decimal string, enums as full SCREAMING_SNAKE wire names, bytes as base64) —
 * so every downstream scenario/fixture keeps its assertions unchanged; only the import path moves.
 * Internally this class adapts SpawnClient's protobuf-es messages (numeric enums, bigint, Uint8Array)
 * back into those wire-shaped DTOs.
 */

import {
  SpawnClient,
  createTransport,
  cpv1,
  type KeyStore,
  type ResolvedTarget,
} from "@spawnery/client";
import { X509Certificate } from "node:crypto";
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
  image?: string;
  runnableId?: string;
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

/** TokenSource is a static bearer (dev-token) or an async provider called fresh per request
 * (OAuth-PoP, whose token expires and must be proactively refreshed — see auth/oauthpop.ts). */
export type TokenSource = string | (() => Promise<string>);

export interface AcceptanceClientOptions {
  baseUrl: string;
  bearer: TokenSource;
  /** Omit for read/delete-only oracle use. Lifecycle callers must provide the SDK's complete
   * node-authorization configuration; a key store alone is deliberately not sufficient. */
  keyStore?: KeyStore;
  getNodeAccessToken?: () => Promise<string>;
  verifyTarget?: (target: ResolvedTarget) => Promise<void>;
}

export interface KnownVMTargetPins {
  rootCAPEM: string;
  trustDomain: string;
  expectedNodeId: string;
  expectedNodeClass: string;
  expectedNodeAccountId: string;
  now?: () => Date;
}

function certificatePEMs(raw: string): string[] {
  return raw.match(/-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----/g) ?? [];
}

/** Strict verifier for the disposable VM's one known node; it signs nothing on mismatch. */
export function createKnownVMTargetVerifier(pins: KnownVMTargetPins) {
  if (!pins.rootCAPEM.trim() || !pins.trustDomain || !pins.expectedNodeId ||
      !pins.expectedNodeClass || !pins.expectedNodeAccountId) {
    throw new Error("known VM target verifier requires root, trust domain, node id, class, and account");
  }
  const root = new X509Certificate(pins.rootCAPEM);
  return async (target: ResolvedTarget): Promise<void> => {
    if (target.targetNodeId !== pins.expectedNodeId || target.targetNodeClass !== pins.expectedNodeClass ||
        target.targetNodeAccountId !== pins.expectedNodeAccountId) {
      throw new Error("known VM target typed identity mismatch");
    }
    const certs = certificatePEMs(new TextDecoder().decode(target.nodeCertChain)).map((pem) => new X509Certificate(pem));
    if (certs.length === 0) throw new Error("known VM target certificate chain is missing");
    const now = (pins.now?.() ?? new Date()).getTime();
    for (const cert of certs) {
      if (now < Date.parse(cert.validFrom) || now > Date.parse(cert.validTo)) {
        throw new Error("known VM target certificate is outside its validity window");
      }
    }
    for (let i = 0; i + 1 < certs.length; i++) {
      if (!certs[i].verify(certs[i + 1].publicKey)) throw new Error("known VM target chain signature is invalid");
    }
    if (!certs.at(-1)!.verify(root.publicKey)) throw new Error("known VM target chain is not rooted in the pinned CA");

    const subjectAltName = certs[0]!.subjectAltName;
    if (!subjectAltName) throw new Error("known VM target certificate has no subject alternative name");
    const uris = [...subjectAltName.matchAll(/URI:([^,\n]+)/g)].map((match) => match[1]);
    if (uris.length !== 1) throw new Error("known VM target must have exactly one URI SAN");
    const uri = new URL(uris[0]!);
    const segments = uri.pathname.split("/").filter(Boolean);
    if (uri.protocol !== "spiffe:" || uri.hostname !== pins.trustDomain || segments.length !== 4 ||
        segments[0] !== "node" || segments[1] !== pins.expectedNodeClass ||
        segments[2] !== pins.expectedNodeAccountId || segments[3] !== pins.expectedNodeId) {
      throw new Error("known VM target SPIFFE principal mismatch");
    }
  };
}

function bytesToB64(b: Uint8Array): string {
  return Buffer.from(b).toString("base64");
}

function b64ToBytes(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, "base64"));
}

/** enumToWire converts a protobuf-es numeric enum value back to its full wire name (protobuf-es
 * strips the enum's own SCREAMING_SNAKE prefix off member names — see cpv1.ProfileEntryKind — so
 * this re-adds it), matching the old Connect-JSON ApiDriver's string enums exactly. NOTE:
 * SpawnStatus is the one enum that must NOT go through this — the driver's SpawnStatus type
 * (drivers/types.ts) and every comparison site expect the bare, unprefixed member name (matching
 * the old driver's stripStatusPrefix), so toSpawnSummary indexes cpv1.SpawnStatus directly. */
function enumToWire(enumObj: Record<number, string>, prefix: string, value: number): string {
  return `${prefix}_${enumObj[value]}`;
}

/** wireToEnum is enumToWire's inverse, for building write requests from a wire-shaped DTO.
 * Typed `object` (not Record<string, number>) because TS's numeric-enum objects also carry a
 * reverse (number -> string) index, which is incompatible with a plain string->number Record. */
function wireToEnum(enumObj: object, prefix: string, wire: string): number {
  const short = wire.startsWith(`${prefix}_`) ? wire.slice(prefix.length + 1) : wire;
  const value = (enumObj as Record<string, unknown>)[short];
  if (typeof value !== "number") throw new Error(`unknown ${prefix} wire value: ${wire}`);
  return value;
}

function toSpawnSummary(s: cpv1.SpawnSummary): SpawnSummary {
  return {
    spawnId: s.spawnId,
    appId: s.appId,
    appVersion: s.appVersion,
    model: s.model,
    status: cpv1.SpawnStatus[s.status] as SpawnStatus,
    createdAt: String(s.createdAt),
    lastUsedAt: String(s.lastUsedAt),
    name: s.name,
    modelApplied: s.modelApplied,
    parentSpawnId: s.parentSpawnId || undefined,
  };
}

function toAppSummary(a: cpv1.AppSummary): AppSummary {
  return { id: a.id, displayName: a.displayName, latestVersion: a.latestVersion, listed: a.listed };
}

function toProfileSummary(p: cpv1.ProfileSummary): ProfileSummary {
  return { profileId: p.profileId, name: p.name, version: Number(p.version), updatedAt: String(p.updatedAt) };
}

function toProfileEntry(e: cpv1.ProfileEntry): ProfileEntry {
  return {
    entryId: e.entryId,
    kind: enumToWire(cpv1.ProfileEntryKind, "PROFILE_ENTRY_KIND", e.kind),
    name: e.name,
    source: enumToWire(cpv1.ProfileEntrySource, "PROFILE_ENTRY_SOURCE", e.source),
    catalogId: e.catalogId || undefined,
    customInline: e.customInline.length > 0 ? bytesToB64(e.customInline) : undefined,
    targets: e.targets,
    mcpSecretRefs: e.mcpSecretRefs,
  };
}

function toProfile(p: cpv1.Profile): Profile {
  return {
    profileId: p.profileId,
    name: p.name,
    version: Number(p.version),
    updatedAt: String(p.updatedAt),
    entries: p.entries.map(toProfileEntry),
    secretIds: p.secretIds,
  };
}

function toCatalogEntrySummary(e: cpv1.CatalogEntrySummary): CatalogEntrySummary {
  return {
    catalogId: e.catalogId,
    kind: enumToWire(cpv1.ProfileEntryKind, "PROFILE_ENTRY_KIND", e.kind),
    name: e.name,
    description: e.description,
  };
}

function toCatalogEntry(e: cpv1.CustomizationCatalogEntry): CatalogEntry {
  return {
    catalogId: e.catalogId,
    creatorId: e.creatorId,
    kind: enumToWire(cpv1.ProfileEntryKind, "PROFILE_ENTRY_KIND", e.kind),
    name: e.name,
    description: e.description,
    content: e.content.length > 0 ? bytesToB64(e.content) : undefined,
    listed: e.listed,
    createdAt: String(e.createdAt),
    updatedAt: String(e.updatedAt),
  };
}

/** SecretCommonFields is the field subset cp.v1.Secret and cp.v1.SecretSummary share — toSecretSummary
 * takes this instead of the branded SecretSummary message type so it can also project a full Secret
 * (toSecretDetail's input) down to the common summary fields. */
interface SecretCommonFields {
  secretId: string;
  type: cpv1.UserSecretType;
  name: string;
  provider: string;
  targetContainer: cpv1.ArtifactTarget;
  envVarName: string;
  destPath: string;
  version: bigint;
  devicesetEpoch: bigint;
  updatedAt: bigint;
}

function toSecretSummary(s: SecretCommonFields): SecretSummary {
  return {
    secretId: s.secretId,
    type: enumToWire(cpv1.UserSecretType, "USER_SECRET_TYPE", s.type),
    name: s.name,
    provider: s.provider || undefined,
    targetContainer: enumToWire(cpv1.ArtifactTarget, "ARTIFACT_TARGET", s.targetContainer),
    envVarName: s.envVarName || undefined,
    destPath: s.destPath || undefined,
    version: Number(s.version),
    devicesetEpoch: Number(s.devicesetEpoch),
    updatedAt: String(s.updatedAt),
  };
}

function toSecretDetail(s: cpv1.Secret): SecretDetail {
  return {
    ...toSecretSummary(s),
    envelope: s.envelope.length > 0 ? bytesToB64(s.envelope) : undefined,
    createdAt: String(s.createdAt),
  };
}

export class AcceptanceClient {
  private readonly sdk: SpawnClient;

  constructor(opts: AcceptanceClientOptions) {
    const transport = createTransport({
      baseUrl: opts.baseUrl,
      auth: {
        getBearer: () => (typeof opts.bearer === "string" ? Promise.resolve(opts.bearer) : opts.bearer()),
      },
    });
    this.sdk = new SpawnClient({
      transport,
      keyStore: opts.keyStore,
      getNodeAccessToken: opts.getNodeAccessToken,
      verifyTarget: opts.verifyTarget,
    });
  }

  // --- Spawns ---

  async createSpawn(req: CreateSpawnApiRequest): Promise<string> {
    return this.sdk.createSpawn({
      appId: req.appId,
      model: req.model ?? "",
      name: req.name ?? "",
      version: req.version ?? "",
      profileId: req.profileId ?? "",
      image: req.image ?? "",
      runnableId: req.runnableId ?? "",
    });
  }

  async listSpawns(): Promise<SpawnSummary[]> {
    return (await this.sdk.list()).map(toSpawnSummary);
  }

  /** findSpawn reads back a single spawn's status via ListSpawns + filter (no GetSpawn RPC exists). */
  async findSpawn(spawnId: string): Promise<SpawnSummary | undefined> {
    const spawns = await this.listSpawns();
    return spawns.find((s) => s.spawnId === spawnId);
  }

  async listApps(query = ""): Promise<AppSummary[]> {
    return (await this.sdk.listApps(query)).apps.map(toAppSummary);
  }

  /** deleteSpawn defaults destroyData to true (overriding the SDK's false) — the sweep/registry
   * cleanup paths depend on a delete actually reclaiming storage, not just stopping the pod. */
  async deleteSpawn(spawnId: string, destroyData = true): Promise<void> {
    await this.sdk.deleteSpawn(spawnId, destroyData);
  }

  async stopSpawn(spawnId: string): Promise<void> {
    await this.sdk.stopSpawn(spawnId);
  }

  // --- Profiles (oracle reads; cross-check for the cli-primary ProfileCli) ---

  async listProfiles(): Promise<ProfileSummary[]> {
    return (await this.sdk.listProfiles()).map(toProfileSummary);
  }

  async getProfile(profileId: string): Promise<Profile> {
    const profile = await this.sdk.getProfile(profileId);
    if (!profile) throw new Error(`GetProfile(${profileId}): empty response`);
    return toProfile(profile);
  }

  // --- Catalog entries (oracle reads; cross-check for the cli-primary CatalogCli) ---

  async listCatalogEntries(): Promise<CatalogEntrySummary[]> {
    return (await this.sdk.listCatalogEntries()).map(toCatalogEntrySummary);
  }

  async getCatalogEntry(catalogId: string): Promise<CatalogEntry> {
    const entry = await this.sdk.getCatalogEntry(catalogId);
    if (!entry) throw new Error(`GetCatalogEntry(${catalogId}): empty response`);
    return toCatalogEntry(entry);
  }

  // --- Secrets (oracle-only: spawnctl has no `secret` subcommand — CLI parity gap, sp-tq0t) ---

  async createSecret(write: SecretWrite): Promise<SecretDetail> {
    const secret = await this.sdk.createSecret({
      secretId: write.secretId,
      type: wireToEnum(cpv1.UserSecretType, "USER_SECRET_TYPE", write.type),
      name: write.name,
      provider: write.provider ?? "",
      targetContainer: wireToEnum(cpv1.ArtifactTarget, "ARTIFACT_TARGET", write.targetContainer),
      envVarName: write.envVarName ?? "",
      destPath: write.destPath ?? "",
      devicesetEpoch: BigInt(write.devicesetEpoch ?? 0),
      envelope: b64ToBytes(write.envelope),
    });
    if (!secret) throw new Error(`CreateSecret(${write.secretId}): empty response`);
    return toSecretDetail(secret);
  }

  async listSecrets(): Promise<SecretSummary[]> {
    return (await this.sdk.listSecrets()).map(toSecretSummary);
  }

  async getSecret(secretId: string): Promise<SecretDetail> {
    const secret = await this.sdk.getSecret(secretId);
    if (!secret) throw new Error(`GetSecret(${secretId}): empty response`);
    return toSecretDetail(secret);
  }

  async deleteSecret(secretId: string): Promise<void> {
    await this.sdk.deleteSecret(secretId);
  }
}
