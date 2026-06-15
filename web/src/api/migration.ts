/**
 * Migration API — Move-to orchestration (sp-8dkp Phase 7).
 *
 * Mirrors cmd/spawnctl/move.go (runMove) for the browser owner client:
 *   1. Fetch owner-sealed journal-key ciphertext (CP holds ciphertext only).
 *   2. Drive MigrateSpawn (suspend source → resume on target).
 *   3. Fetch target node key material + verify via PKI (verifyNodeForSealing).
 *   4. Unseal each journal key with this device's X25519 key, re-seal to the
 *      target node's HPKE sub-key under the in-flight AAD.
 *   5. Deliver the resealed ciphertext to the CP (which relays to the node).
 *
 * Intent flow: the state machine (useMoveTo.ts) launches pollAndSign concurrently;
 * this module does not touch the intent layer directly.
 *
 * Design: docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §3
 */

import { unary } from "./connect";
import { authEnabled, useSessionStore } from "@/auth/session";
import { pollAndSign, registerPendedOp, clearPendedOp } from "@/auth/intent";
import { openEnvelope, hpkeSeal } from "@/keys/hpke";
import type { Envelope } from "@/keys/hpke";
import { asNodeRevocationChecker } from "@/keys/nodeRevocation";
import { verifyNodeForSealing } from "@/keys/subkey";
import type { RevocationChecker } from "@/keys/subkey";
import type { DeviceKeys } from "@/keys/device";
import { fromBase64, toBase64, encodeFields } from "@/keys/encoding";

// ── Typed migration errors ────────────────────────────────────────────────────

/**
 * MigrateError carries a per-leg tag so the UI state machine can show distinct
 * recovery actions (spec §3 "Errors — split by leg", WM3).
 *
 *   "suspend"  — MigrateSpawn failed while suspending the source; spawn is in error state.
 *   "resume"   — MigrateSpawn suspended but failed to resume; spawn is in suspended state.
 *   "delivery" — key delivery failed after migrate succeeded; persistent reload-derivable state.
 *   "network"  — CP unreachable (offline); caller should retry with backoff.
 */
export class MigrateError extends Error {
  constructor(
    message: string,
    public readonly leg: "suspend" | "resume" | "delivery" | "network",
  ) {
    super(message);
    this.name = "MigrateError";
  }
}

// ── Public types ──────────────────────────────────────────────────────────────

/** One candidate node for a spawn migration (mirrors proto MigrationTarget). */
export interface MigrationTarget {
  nodeId:           string;
  class:            string;   // "cloud" | "self-hosted"
  yours:            boolean;  // owned by the requesting user
  online:           boolean;  // heartbeated within the live window
  isCurrent:        boolean;  // this is the spawn's current hosting node
  journalSizeBytes: number;   // best-effort (0 = unknown)
}

/** One mount's owner-sealed journal-password ciphertext (opaque at-rest envelope). */
export interface JournalEntry {
  mount:      string;       // mount name (label)
  ciphertext: string;       // base64-encoded JSON seal.Envelope
}

/** Node key material relayed by the CP (from GetSpawnNodeKey). */
export interface NodeKey {
  nodeCertChain: string; // PEM text (base64-decoded from bytes field)
  signedSubkey:  string; // JSON text (base64-decoded from bytes field)
  generation:    number;
}

type DeliverySecret = {
  secretId:   string;
  targetPath: string;
  sealed:     string;
  version:    number;
  deliveryId: string;
};

/**
 * Durability of a spawn's storage, from the owner client's perspective.
 * "ephemeral"     – scratch storage only, no journal; migration is data-loss-free.
 * "owner-sealed"  – journaled mounts with owner-sealed ciphertext in the CP;
 *                   full migration with journal key delivery is possible.
 * "node-local"    – journaled mounts but NOT yet owner-sealed;
 *                   MigrateSpawn is blocked until UpgradeToOwnerSealed is run.
 */
export type DurabilityClass = "ephemeral" | "owner-sealed" | "node-local";

/** Per-step result from runMigrate (progress callbacks). */
export interface MigrateProgress {
  step: "fetching-keys" | "migrating" | "verifying-node" | "resealing" | "delivering" | "done";
  /** Resolved target node ID (set from "migrating" step onward). */
  resolvedNodeId?: string;
  /** Number of journal keys delivered (set at "done"). */
  journalKeysDelivered?: number;
}

/** Full runMigrate result on success. */
export interface MigrateResult {
  resolvedNodeId:     string;
  transferSetId:      string;
  journalKeysDelivered: number;
}

// ── RPC wrappers ──────────────────────────────────────────────────────────────

/** Result of listMigrationTargets: target list + CP-computed spawn durability class. */
export interface MigrationTargetsData {
  targets:              MigrationTarget[];
  /** CP-resolved durability class: "ephemeral" | "node-local" | "owner-sealed" */
  spawnDurabilityClass: string;
}

/** listMigrationTargets enumerates candidate nodes for a spawn migration. */
export async function listMigrationTargets(spawnId: string): Promise<MigrationTargetsData> {
  const r = await unary<{
    targets?: Array<{
      nodeId?: string; class?: string; yours?: boolean; online?: boolean;
      isCurrent?: boolean; journalSizeBytes?: string;
    }>;
    spawnDurabilityClass?: string;
  }>("ListMigrationTargets", { spawnId });
  const targets = (r.targets ?? []).map((t) => ({
    nodeId:           t.nodeId          ?? "",
    class:            t.class           ?? "",
    yours:            !!t.yours,
    online:           !!t.online,
    isCurrent:        !!t.isCurrent,
    // uint64 arrives as decimal string in Connect-JSON; treat as number (safe for reasonable sizes).
    journalSizeBytes: t.journalSizeBytes ? Number(t.journalSizeBytes) : 0,
  }));
  return { targets, spawnDurabilityClass: r.spawnDurabilityClass ?? "" };
}

/** getJournalKeyCiphertext fetches the owner-sealed journal-password ciphertext for a spawn. */
export async function getJournalKeyCiphertext(spawnId: string): Promise<JournalEntry[]> {
  const r = await unary<{
    entries?: Array<{ mount?: string; ciphertext?: string }>
  }>("GetJournalKeyCiphertext", { spawnId });
  return (r.entries ?? []).map((e) => ({
    mount:      e.mount      ?? "",
    ciphertext: e.ciphertext ?? "",
  }));
}

/** getSpawnNodeKey fetches the hosting node's relayed key material. */
export async function getSpawnNodeKey(spawnId: string): Promise<NodeKey> {
  const r = await unary<{
    nodeCertChain?: string;
    signedSubkey?:  string;
    generation?:    string;
  }>("GetSpawnNodeKey", { spawnId });
  // bytes fields arrive as base64; decode to text (PEM / JSON).
  const nodeCertChain = r.nodeCertChain
    ? new TextDecoder().decode(fromBase64(r.nodeCertChain)) : "";
  const signedSubkey = r.signedSubkey
    ? new TextDecoder().decode(fromBase64(r.signedSubkey)) : "";
  return {
    nodeCertChain,
    signedSubkey,
    generation: r.generation ? Number(r.generation) : 0,
  };
}

/** deliverSecrets hands the CP owner-sealed ciphertext to relay to the spawn's live node. */
async function deliverSecrets(
  spawnId: string,
  secrets: DeliverySecret[],
): Promise<void> {
  await unary<Record<string, never>>("DeliverSecrets", { spawnId, secrets });
}

/**
 * upgradeToOwnerSealed asks the CP to instruct the spawn's hosting node to seal
 * the existing node-local repo password to the owner's device set. After this call
 * succeeds a subsequent MigrateSpawn with upgrade_to_owner_sealed=true is accepted.
 *
 * ownerDevicePubkeys are the raw X25519 pubkeys (32 bytes each) of all enrolled devices;
 * passing null lets the node choose its own sub-key (for self-upgrade without re-seal).
 *
 * Mirrors UpgradeToOwnerSealed proto RPC (proto/cp/v1/cp.proto:46).
 */
export async function upgradeToOwnerSealed(
  spawnId: string,
  ownerDevicePubkeys: Uint8Array[],
): Promise<void> {
  // Connect-JSON encodes repeated bytes as base64 strings.
  const pubkeysB64 = ownerDevicePubkeys.map(toBase64);
  await unary<Record<string, never>>("UpgradeToOwnerSealed", {
    spawnId,
    ownerDevicePubkeys: pubkeysB64,
  });
}

/** migrateSpawnRPC drives the MigrateSpawn RPC and returns the resolved node ID + transfer set. */
async function migrateSpawnRPC(
  spawnId: string,
  targetNodeId: string,
  targetClass: string,
  upgradeToOwnerSealed: boolean,
): Promise<{ nodeId: string; transferSetId: string }> {
  const r = await unary<{ nodeId?: string; transferSetId?: string }>("MigrateSpawn", {
    spawnId,
    targetNodeId,
    targetClass,
    upgradeToOwnerSealed,
  });
  return { nodeId: r.nodeId ?? "", transferSetId: r.transferSetId ?? "" };
}

// ── Durability classification ─────────────────────────────────────────────────

/**
 * classifyDurability classifies a spawn's storage durability.
 *
 * - "owner-sealed":  journal ciphertext entries exist → full migration with key delivery
 * - "node-local":    CP reports node-local journal (no owner-sealed ciphertext yet)
 *                    → must UpgradeToOwnerSealed before migrating (WM16)
 * - "ephemeral":     no journal
 *
 * spawnDurabilityClass is the authoritative CP-reported class (from ListMigrationTargets).
 * It takes precedence over journalSizeBytes (which the CP hardcodes to 0, sp-e642).
 * journalEntries is checked first: if entries exist, the spawn is definitively owner-sealed.
 */
export function classifyDurability(
  journalEntries: JournalEntry[],
  targets: MigrationTarget[],
  spawnDurabilityClass: string,
): DurabilityClass {
  if (journalEntries.length > 0) return "owner-sealed";
  // Use the CP-authoritative class when available.
  if (spawnDurabilityClass === "node-local") return "node-local";
  if (spawnDurabilityClass === "owner-sealed") return "owner-sealed";
  // Fall back to journalSizeBytes heuristic (sp-e642: currently always 0 from CP).
  const current = targets.find((t) => t.isCurrent);
  if (current && current.journalSizeBytes > 0) return "node-local";
  return "ephemeral";
}

// ── In-flight AAD encoding ────────────────────────────────────────────────────

interface InFlightAAD {
  spawnId:    string;
  generation: number;
  nodeId:     string;
  notAfter:   Date;   // sub-key expiry; used as u64 UnixNano in AAD
  version:    number; // == generation (mirrors Go's aad.Version = nk.Msg.Generation)
  deliveryId: string; // one-time UUID (replay defence)
}

/** Encode in-flight AAD bytes, mirroring Go's seal.InFlightAAD.bytes(). */
function inFlightAADBytes(aad: InFlightAAD): Uint8Array {
  const enc = new TextEncoder();
  // NotAfter as UnixNano (int64 → BigInt); must use BigInt to avoid precision loss (WM10).
  const notAfterNs = BigInt(aad.notAfter.getTime()) * 1_000_000n;
  return encodeFields(
    enc.encode("in-flight/v1"),
    enc.encode(aad.spawnId),
    _u64big(BigInt(aad.generation)),
    enc.encode(aad.nodeId),
    _u64big(notAfterNs),
    _u64big(BigInt(aad.version)),
    enc.encode(aad.deliveryId),
  );
}

/** Encode a BigInt as 8-byte big-endian (WM10: UnixNano exceeds Number.MAX_SAFE_INTEGER). */
function _u64big(v: bigint): Uint8Array {
  const b = new Uint8Array(8);
  new DataView(b.buffer).setBigUint64(0, v, false);
  return b;
}

// ── Per-mount reseal ──────────────────────────────────────────────────────────

/**
 * reSealToNode opens an owner-sealed Envelope with the device's X25519 key and
 * re-seals the recovered plaintext to the target node's HPKE sub-key under the
 * in-flight AAD. Returns base64-encoded JSON NodeSealed bytes suitable for the
 * SealedSecret.sealed bytes field (proto bytes → base64 in Connect-JSON).
 *
 * Mirrors Go's journalkey.ResealForNode / seal.ReSealToNode.
 */
async function reSealToNode(
  ciphertextB64: string,
  deviceKeys: DeviceKeys,
  nodeHPKEPub: Uint8Array,
  aad: InFlightAAD,
): Promise<string> {
  // Decode ciphertext: CP stores JSON-encoded seal.Envelope; Connect-JSON wraps bytes as base64.
  const envelopeBytes = fromBase64(ciphertextB64);
  const envelopeJSON  = new TextDecoder().decode(envelopeBytes);
  const env: Envelope = JSON.parse(envelopeJSON);

  // Export raw public bytes for openEnvelope (trial-open needs the pubkey hint comparison).
  const xPubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", deviceKeys.x25519Public));

  // Open the at-rest envelope with the device's non-extractable X25519 private key.
  const payload = await openEnvelope(env, deviceKeys.x25519Private, xPubRaw);

  // Re-seal the payload to the node's HPKE pub key (in-flight info + AAD).
  const inFlightAad = inFlightAADBytes(aad);
  const rs = await hpkeSeal(payload, nodeHPKEPub, inFlightAad, "spawnery/secrets/seal/in-flight/v1");

  // NodeSealed = {enc, ct}; JSON-encode and base64-wrap for the bytes proto field.
  const nodeSealed = { enc: rs.enc, ct: rs.ct };
  const sealedBytes = new TextEncoder().encode(JSON.stringify(nodeSealed));
  return toBase64(sealedBytes);
}

// ── Full migration orchestration ──────────────────────────────────────────────

/**
 * runMigrate orchestrates a full spawn migration (mirrors runMove in move.go):
 *   1. Fetch owner-sealed journal key ciphertext (CP holds ciphertext only).
 *   2. Drive MigrateSpawn (suspend source → resume on target); intent flow is
 *      launched concurrently via the A4 pollAndSign path.
 *   3. Fetch the target node's key material + PKI-verify via verifyNodeForSealing
 *      (including AS revocation check if revocationChecker is supplied).
 *   4. Unseal each journal key with this device key, re-seal to the target node.
 *   5. Deliver the resealed ciphertext to the CP (which relays to the node).
 *
 * Throws MigrateError with a per-leg tag so callers can show distinct recovery actions:
 *   leg="suspend"  — MigrateSpawn failed at source suspend; spawn is in error state.
 *   leg="resume"   — MigrateSpawn suspended but failed to resume; spawn is in suspended state.
 *   leg="delivery" — key delivery failed; spawn is active-but-keyless (persistent, reload-derivable).
 *   leg="network"  — CP unreachable mid-operation; caller should retry with backoff.
 *
 * target: the selected MigrationTarget (nodeId + class from listMigrationTargets).
 *         For cloud class, nodeId may be empty and class = "cloud".
 * deviceKeys: the caller must load and pass the device X25519 keypair.
 * rootPEM: pinned Root CA PEM embedded in the web bundle (empty = dev/insecure mode).
 * now: injectable for testing; use new Date() in production.
 * revocationChecker: optional checker override; default is the AS checker with no cache.
 * onProgress: optional per-step callback for UI progress updates.
 * spawnStatusAfterFail: injectable for testing — resolves to current spawn status after
 *   a migrate-leg failure so the "suspend" vs "resume" leg tag can be set correctly.
 */
export async function runMigrate(
  spawnId:    string,
  target:     Pick<MigrationTarget, "nodeId" | "class">,
  deviceKeys: DeviceKeys,
  rootPEM:    string,
  now:        Date,
  onProgress?: (p: MigrateProgress) => void,
  revocationChecker?: RevocationChecker,
  spawnStatusAfterFail?: (id: string) => Promise<string>,
): Promise<MigrateResult> {
  const targetNodeId  = target.class === "cloud" ? "" : target.nodeId;
  const targetClass   = target.class === "cloud" ? "cloud" : "";

  // Step 1: fetch owner-sealed journal key ciphertext.
  onProgress?.({ step: "fetching-keys" });
  let entries: JournalEntry[];
  try {
    entries = await getJournalKeyCiphertext(spawnId);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new MigrateError(`Failed to fetch journal key ciphertext: ${msg}`, "network");
  }
  // No entries → migration proceeds without journal key delivery (ephemeral or node-local).

  // Step 2: drive MigrateSpawn (suspend source → resume on target).
  onProgress?.({ step: "migrating" });
  let resolvedNodeId: string;
  let transferSetId = "";
  try {
    const migrated = await migrateSpawnRPC(spawnId, targetNodeId, targetClass, entries.length > 0);
    resolvedNodeId = migrated.nodeId;
    transferSetId = migrated.transferSetId;
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    // Classify as suspend-fail or resume-fail by checking spawn status after failure.
    // If we can't determine (offline), default to "network".
    let leg: "suspend" | "resume" | "network" = "network";
    const checkStatus = spawnStatusAfterFail ?? _defaultSpawnStatusCheck;
    try {
      const st = await checkStatus(spawnId);
      if (st === "error" || st === "unreachable") leg = "suspend";
      else if (st === "suspended") leg = "resume";
      else leg = "network";
    } catch { /* status check failed — offline */ }
    throw new MigrateError(`Migration failed: ${msg}`, leg);
  }

  // Intent signing: mirrors spawnlet.migrateSpawn.
  if (authEnabled()) {
    const { getOrCreateSessionKey } = await import("@/auth/keypair");
    const kp = await getOrCreateSessionKey(useSessionStore.getState().keyStore);
    const pended = { op: "migrate-spawn", spawnId };
    registerPendedOp(pended);
    pollAndSign({ spawnId, pended, privateKey: kp.privateKey, publicKey: kp.publicKey })
      .catch((e: unknown) => console.error("intent sign failed:", e))
      .finally(() => clearPendedOp(spawnId));
  }

  if (entries.length === 0) {
    // No journal mounts; migration is complete.
    onProgress?.({ step: "done", resolvedNodeId, journalKeysDelivered: 0 });
    return { resolvedNodeId, transferSetId, journalKeysDelivered: 0 };
  }

  // Step 3: fetch the target node's key material and verify.
  onProgress?.({ step: "verifying-node", resolvedNodeId });
  let nk: NodeKey;
  try {
    nk = await getSpawnNodeKey(spawnId);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new MigrateError(`Failed to fetch node key: ${msg}`, "delivery");
  }

  // Parse the sub-key JSON (we need not_after for the in-flight AAD).
  const sk = JSON.parse(nk.signedSubkey) as { node_id: string; not_after: string };
  const notAfter = new Date(sk.not_after);

  // verifyNodeForSealing returns the trusted HPKE pubkey, or throws on PKI failure
  // (including AS revocation check — fail-closed on any checker error, WM8).
  const tenancy = (target.class === "cloud" ? "cloud" : "self-hosted") as "cloud" | "self-hosted";
  // For self-hosted targets, accountId is required: the owner's account must match the node's SAN.
  const accountId = tenancy === "self-hosted"
    ? (useSessionStore.getState().account?.accountId ?? "")
    : undefined;
  const checker = revocationChecker ?? asNodeRevocationChecker();
  let hpkePub: Uint8Array;
  try {
    const verified = await verifyNodeForSealing(
      nk.nodeCertChain,
      rootPEM,
      nk.signedSubkey,
      { tenancy, accountId },
      now,
      checker,
    );
    hpkePub = verified.hpkePub;
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new MigrateError(`Node verification failed: ${msg}`, "delivery");
  }

  // Step 4: unseal + re-seal each journal key to the target node.
  onProgress?.({ step: "resealing", resolvedNodeId });
  const secrets: DeliverySecret[] = [];
  try {
    for (const entry of entries) {
      const aad: InFlightAAD = {
        spawnId,
        generation: nk.generation,
        nodeId:     resolvedNodeId,
        notAfter,
        version:    nk.generation,
        deliveryId: crypto.randomUUID(),
      };
      const sealedB64 = await reSealToNode(entry.ciphertext, deviceKeys, hpkePub, aad);
      const secretId = `journal/${entry.mount}`;
      secrets.push({ secretId, targetPath: secretId, sealed: sealedB64, version: aad.version, deliveryId: aad.deliveryId });
    }
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new MigrateError(`Re-seal failed: ${msg}`, "delivery");
  }

  // Step 5: deliver the resealed ciphertext.
  onProgress?.({ step: "delivering", resolvedNodeId });
  try {
    await deliverSecrets(spawnId, secrets);
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new MigrateError(`Key delivery failed: ${msg}`, "delivery");
  }

  onProgress?.({ step: "done", resolvedNodeId, journalKeysDelivered: secrets.length });
  return { resolvedNodeId, transferSetId, journalKeysDelivered: secrets.length };
}

/** Default spawn status check used by runMigrate to classify suspend vs resume failures. */
async function _defaultSpawnStatusCheck(spawnId: string): Promise<string> {
  const { listSpawns } = await import("./spawnlet");
  const spawns = await listSpawns();
  return spawns.find((s) => s.spawnId === spawnId)?.status ?? "unknown";
}
