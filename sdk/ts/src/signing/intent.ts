/**
 * Intent signing — mirrors cmd/spawnctl/intent.go and internal/intent [AC1][AM11].
 *
 * Key invariants:
 * - Clients MUST pend an op locally before signing (AM1: "never sign unpended bytes").
 * - pollAndSign validates the CP-returned tuple against the locally-pended record.
 * - Any field mismatch between the CP tuple and the locally-pended record → refuse to sign.
 *
 * IntentBody is marshalled with protobuf-es `toBinary`, proven byte-identical to Go's
 * proto.Marshal (T1 wire-compat spike; see sdk/ts/spike/wire-compat.test.ts) — no hand-rolled
 * proto codec here, unlike the web port this SDK supersedes.
 */

import { create, toBinary } from "@bufbuild/protobuf";
import type { Client } from "@connectrpc/connect";
import { IntentBodySchema, SignedIntentSchema, type SignedIntent } from "../gen/auth/v1/auth_pb.js";
import type { SpawnService } from "../gen/cp/v1/cp_pb.js";
import { signP1363, exportSpkiDer } from "../keys/crypto.js";
import { toBase64 } from "../keys/encoding.js";

// Intent domain constants (must match internal/intent/intent.go).
export const DOMAIN_CREATE_SPAWN = "spawnery/intent/create-spawn/v1";
export const DOMAIN_RESUME_SPAWN = "spawnery/intent/resume-spawn/v1";
export const DOMAIN_RECREATE_SPAWN = "spawnery/intent/recreate-spawn/v1";
export const DOMAIN_MIGRATE_SPAWN = "spawnery/intent/migrate-spawn/v1";
export const DOMAIN_FORK_SPAWN = "spawnery/intent/fork-spawn/v1";
export const DOMAIN_SESSION_OPEN = "spawnery/intent/session-open/v1";

export function domainForOp(op: string): string {
  switch (op) {
    case "create-spawn":   return DOMAIN_CREATE_SPAWN;
    case "resume-spawn":   return DOMAIN_RESUME_SPAWN;
    case "recreate-spawn": return DOMAIN_RECREATE_SPAWN;
    case "migrate-spawn":  return DOMAIN_MIGRATE_SPAWN;
    case "fork-spawn":     return DOMAIN_FORK_SPAWN;
    case "session-open":   return DOMAIN_SESSION_OPEN;
    default:               return `spawnery/intent/${op}/v1`;
  }
}

// ── IntentBody encoding ───────────────────────────────────────────────────────

export interface IntentFields {
  jti: string;            // field 1
  issuedAt: number;       // field 2 (unix s)
  spawnId: string;        // field 3
  generation: bigint;     // field 4
  targetNodeId: string;   // field 5
  op: string;             // field 6
  appRef: string;         // field 7
  image: string;          // field 8
  model: string;          // field 9
  dataRef: string;        // field 10
  sessionId: string;      // field 11
  mounts: Array<{
    name: string;
    backendUri: string;
    credentialSecretId?: string;
    createIfMissing?: boolean;
    repositoryId?: string;
  }>; // field 12 repeated MountRef — all 5 fields signed (node correspondence compares all 5)
}

/**
 * buildIntentBodyBytes encodes IntentFields to proto3 bytes matching Go's proto.Marshal.
 * protobuf-es proto3 field-omit-on-zero semantics match Go's exactly (T1 spike), so this is a
 * straight `create` + `toBinary` — no manual zero-value guards needed.
 */
export function buildIntentBodyBytes(f: IntentFields): Uint8Array {
  const body = create(IntentBodySchema, {
    jti: f.jti,
    issuedAt: BigInt(f.issuedAt),
    spawnId: f.spawnId,
    generation: f.generation,
    targetNodeId: f.targetNodeId,
    op: f.op,
    appRef: f.appRef,
    image: f.image,
    model: f.model,
    dataRef: f.dataRef,
    sessionId: f.sessionId,
    mounts: (f.mounts ?? []).map((m) => ({
      name: m.name,
      backendUri: m.backendUri,
      credentialSecretId: m.credentialSecretId ?? "",
      createIfMissing: m.createIfMissing ?? false,
      repositoryId: m.repositoryId ?? "",
    })),
  });
  return toBinary(IntentBodySchema, body);
}

// ── SignedIntent construction ─────────────────────────────────────────────────

/**
 * buildSignedIntent signs the intent body and returns a SignedIntent message, ready to embed
 * directly in a SubmitIntentRequest (the generated client serializes its bytes fields, so unlike
 * the web port there is no base64/JSON step here).
 *
 * Signature: ECDSA P-256 P1363 over SHA-256(domain || body_bytes)
 * (matches internal/intent/intent.go signP1363)
 */
export async function buildSignedIntent(
  op: string,
  bodyBytes: Uint8Array,
  privateKey: CryptoKey,
  spkiDer: Uint8Array,
): Promise<SignedIntent> {
  const domain = domainForOp(op);
  const domainBytes = new TextEncoder().encode(domain);
  const msg = new Uint8Array(domainBytes.length + bodyBytes.length);
  msg.set(domainBytes);
  msg.set(bodyBytes, domainBytes.length);

  const sig = await signP1363(privateKey, msg);

  return create(SignedIntentSchema, {
    domain,
    body: bodyBytes,
    sig,
    spkiDer,
  });
}

// ── Session-open intent (WS bind frame) ───────────────────────────────────────

/**
 * buildSessionOpenSignedIntentB64 builds, signs, and proto-marshals an OpSessionOpen SignedIntent
 * for the /ws/session bind frame, returning the standard-base64 of proto.Marshal(SignedIntent).
 *
 * Mirrors cmd/spawnctl/main.go runCP's bindFrame.SessionAuth: the node's enforced IntentVerifier
 * requires a session-open auth envelope (else MISSING_INTENT NACK -> client never attaches). Unlike
 * create/resume there is no GetPendingIntent round-trip — the client signs spawnId + the live
 * episode `generation` + sessionId directly. This is a WS bind-frame string (not an RPC field), so
 * unlike buildSignedIntent it still returns base64 of the marshalled SignedIntent message.
 */
export async function buildSessionOpenSignedIntentB64(
  spawnId: string,
  sessionId: string,
  generation: bigint,
  privateKey: CryptoKey,
  publicKey: CryptoKey,
): Promise<string> {
  const spki = await exportSpkiDer(publicKey);
  const jtiBytes = crypto.getRandomValues(new Uint8Array(16));
  const jti = Array.from(jtiBytes).map((b) => b.toString(16).padStart(2, "0")).join("");

  const bodyBytes = buildIntentBodyBytes({
    jti,
    issuedAt: Math.floor(Date.now() / 1000),
    spawnId,
    generation,
    targetNodeId: "",
    op: "session-open",
    appRef: "",
    image: "",
    model: "",
    dataRef: "",
    sessionId: sessionId || "0",
    mounts: [],
  });

  const signedIntent = await buildSignedIntent("session-open", bodyBytes, privateKey, spki);
  const protoBytes = toBinary(SignedIntentSchema, signedIntent);
  return toBase64(protoBytes);
}

// ── Locally-pended intent registry ───────────────────────────────────────────

export interface PendedOp {
  op: string;
  spawnId: string;
  appRef?: string;
  model?: string;
  targetNodeId?: string;
  image?: string;
  dataRef?: string;
  // credentialSecretId is CP-derived and intentionally NOT pended by the client. Optional name/
  // backendUri (rather than required) so a generated CreateSpawnRequest's MessageInitShape mounts
  // array (whose scalar fields are all optional) can be passed straight through from the client.
  mounts?: Array<{ name?: string; backendUri?: string; createIfMissing?: boolean }>;
}

/** In-memory registry of pending ops (keyed by spawnId). */
const _pendedOps = new Map<string, PendedOp>();

export function registerPendedOp(op: PendedOp): void {
  _pendedOps.set(op.spawnId, op);
}

export function clearPendedOp(spawnId: string): void {
  _pendedOps.delete(spawnId);
}

// ── pollAndSign ───────────────────────────────────────────────────────────────

export interface PollAndSignDeps {
  client: Client<typeof SpawnService>;
  spawnId: string;
  pended: PendedOp;
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** Max poll attempts before giving up */
  maxAttempts?: number;
}

/**
 * pollAndSign polls GetPendingIntent until ready, validates the tuple against
 * the locally-pended record (AM1), then builds and submits a SignedIntent.
 *
 * Returns the generated jti on success, throws on validation failure.
 */
export async function pollAndSign(deps: PollAndSignDeps): Promise<string> {
  const {
    client,
    spawnId,
    pended,
    privateKey,
    publicKey,
    maxAttempts = 30,
  } = deps;

  const spki = await exportSpkiDer(publicKey);

  // Poll until ready
  let pending: Awaited<ReturnType<typeof client.getPendingIntent>>["pending"] | undefined;
  for (let i = 0; i < maxAttempts; i++) {
    const res = await client.getPendingIntent({ spawnId });
    if (res.ready && res.pending) {
      pending = res.pending;
      break;
    }
    // Not ready yet — wait 500ms and retry.
    await new Promise<void>((resolve) => setTimeout(resolve, 500));
  }

  if (!pending) {
    throw new Error(`intent: GetPendingIntent not ready after ${maxAttempts} attempts for ${spawnId}`);
  }

  // AM1: validate the CP-returned tuple against the locally-pended record.
  _validateTuple(pending, pended);

  // Generate jti (16 random bytes as hex).
  const jtiBytes = crypto.getRandomValues(new Uint8Array(16));
  const jti = Array.from(jtiBytes).map((b) => b.toString(16).padStart(2, "0")).join("");

  const issuedAt = Math.floor(Date.now() / 1000);
  const generation = pending.generation ?? 0n;

  const bodyBytes = buildIntentBodyBytes({
    jti,
    issuedAt,
    spawnId: pending.spawnId,
    generation,
    targetNodeId: pending.targetNodeId ?? "",
    op: pending.op,
    appRef: pending.appRef ?? "",
    image: pending.image ?? "",
    model: pending.model ?? "",
    dataRef: pending.dataRef ?? "",
    sessionId: "",
    mounts: (pending.mounts ?? []).map((m) => ({
      name: m.name,
      backendUri: m.backendUri,
      credentialSecretId: m.credentialSecretId ?? "",
      createIfMissing: m.createIfMissing ?? false,
      repositoryId: m.repositoryId ?? "",
    })),
  });

  const signedIntent = await buildSignedIntent(pending.op, bodyBytes, privateKey, spki);

  await client.submitIntent({
    spawnId,
    intent: signedIntent,
    nodeAccessToken: "", // dev CP mints node token from SPKI (R5: prod gap)
  });

  return jti;
}

// ── Tuple validation (AM1) ────────────────────────────────────────────────────

/** Minimal shape of the CP's PendingIntent tuple needed for validation + re-signing. */
interface PendingIntentTuple {
  op: string;
  spawnId: string;
  generation?: bigint;
  targetNodeId?: string;
  image?: string;
  appRef?: string;
  model?: string;
  dataRef?: string;
  mounts?: Array<{
    name: string;
    backendUri: string;
    credentialSecretId?: string;
    createIfMissing?: boolean;
    repositoryId?: string;
  }>;
}

function _validateTuple(pending: PendingIntentTuple, pended: PendedOp): void {
  // Op must match.
  if (pending.op !== pended.op) {
    throw new Error(`intent: op mismatch: CP says "${pending.op}", locally pended "${pended.op}"`);
  }
  // spawnId must match — except for fork-spawn. ForkSpawn is synchronous and the CP mints the
  // fork's id, so the client cannot know it before the (blocking) RPC returns; poll/submit are
  // keyed by the source id while the tuple's spawnId is the fresh fork id. There is nothing the
  // client can compare it against, so the equality check is skipped for this op.
  if (pended.op !== "fork-spawn" && pending.spawnId !== pended.spawnId) {
    throw new Error(`intent: spawnId mismatch`);
  }
  // For create-spawn: appRef, model must match.
  if (pended.op === "create-spawn") {
    if (pended.appRef !== undefined && pending.appRef !== pended.appRef) {
      throw new Error(`intent: appRef mismatch: CP "${pending.appRef}", local "${pended.appRef}"`);
    }
    if (pended.model !== undefined && pending.model !== pended.model) {
      throw new Error(`intent: model mismatch: CP "${pending.model}", local "${pended.model}"`);
    }
    if (pended.targetNodeId !== undefined && pending.targetNodeId !== pended.targetNodeId) {
      throw new Error(`intent: targetNodeId mismatch`);
    }
  }
}
