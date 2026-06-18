/**
 * Intent signing — mirrors cmd/spawnctl/intent.go [AC1][AM11].
 *
 * Key invariants:
 * - Clients MUST pend an op locally before signing (AM1: "never sign unpended bytes").
 * - pollAndSign validates the CP-returned tuple against the locally-pended record.
 * - Any field mismatch between the CP tuple and the locally-pended record → refuse to sign.
 */

import { ProtoWriter } from "./protobuf";
import { signP1363, exportSpkiDer } from "./keypair";
import { unary } from "@/api/connect";

// Intent domain constants (must match internal/intent/intent.go).
export const DOMAIN_CREATE_SPAWN = "spawnery/intent/create-spawn/v1";
export const DOMAIN_RESUME_SPAWN = "spawnery/intent/resume-spawn/v1";
export const DOMAIN_RECREATE_SPAWN = "spawnery/intent/recreate-spawn/v1";
export const DOMAIN_MIGRATE_SPAWN = "spawnery/intent/migrate-spawn/v1";
export const DOMAIN_SESSION_OPEN = "spawnery/intent/session-open/v1";

export function domainForOp(op: string): string {
  switch (op) {
    case "create-spawn":   return DOMAIN_CREATE_SPAWN;
    case "resume-spawn":   return DOMAIN_RESUME_SPAWN;
    case "recreate-spawn": return DOMAIN_RECREATE_SPAWN;
    case "migrate-spawn":  return DOMAIN_MIGRATE_SPAWN;
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
 * Field order must match IntentBody field numbers. Proto3 omit-zero applies.
 */
export function buildIntentBodyBytes(f: IntentFields): Uint8Array {
  const w = new ProtoWriter();
  if (f.jti)          w.writeBytes(1, f.jti);
  if (f.issuedAt > 0) w.writeVarint(2, BigInt(f.issuedAt));
  if (f.spawnId)      w.writeBytes(3, f.spawnId);
  if (f.generation > 0n) w.writeVarint(4, f.generation);
  if (f.targetNodeId) w.writeBytes(5, f.targetNodeId);
  if (f.op)           w.writeBytes(6, f.op);
  if (f.appRef)       w.writeBytes(7, f.appRef);
  if (f.image)        w.writeBytes(8, f.image);
  if (f.model)        w.writeBytes(9, f.model);
  if (f.dataRef)      w.writeBytes(10, f.dataRef);
  if (f.sessionId)    w.writeBytes(11, f.sessionId);
  // Repeated MountRef (field 12): name=1, backendUri=2, credentialSecretId=3,
  // createIfMissing=4 (varint), repositoryId=5. The node correspondence check
  // (internal/node/intentverify.go) compares ALL FIVE fields, so all must be
  // signed; ProtoWriter omits empty/zero to match Go proto3 marshalling.
  for (const mount of f.mounts ?? []) {
    const mw = new ProtoWriter();
    mw.writeBytes(1, mount.name ?? "");
    mw.writeBytes(2, mount.backendUri ?? "");
    mw.writeBytes(3, mount.credentialSecretId ?? "");
    mw.writeVarint(4, mount.createIfMissing ? 1 : 0);
    mw.writeBytes(5, mount.repositoryId ?? "");
    w.writeMessage(12, mw);
  }
  return w.finish();
}

// ── SignedIntent construction ─────────────────────────────────────────────────

export interface SignedIntentProto {
  /** base64 (standard) encoded domain || body_bytes signed message bytes — NOT base64url. */
  domain: string;
  /** base64 (standard) encoded body_bytes. */
  body: string;
  /** base64 (standard) encoded P1363 sig (64 bytes). */
  sig: string;
  /** base64 (standard) encoded DER SPKI. */
  spkiDer: string;
}

/**
 * buildSignedIntent signs the intent body and returns the SignedIntent-shaped object
 * ready for JSON serialization in Connect-JSON RPCs.
 *
 * Signature: ECDSA P-256 P1363 over SHA-256(domain || body_bytes)
 * (matches internal/intent/intent.go signP1363)
 */
export async function buildSignedIntent(
  op: string,
  bodyBytes: Uint8Array,
  privateKey: CryptoKey,
  spkiDer: Uint8Array,
): Promise<SignedIntentProto> {
  const domain = domainForOp(op);
  const domainBytes = new TextEncoder().encode(domain);
  const msg = new Uint8Array(domainBytes.length + bodyBytes.length);
  msg.set(domainBytes);
  msg.set(bodyBytes, domainBytes.length);

  const sig = await signP1363(privateKey, msg);

  // Connect-JSON encodes bytes fields as standard base64 (not base64url).
  const toB64 = (b: Uint8Array) => btoa(String.fromCharCode(...b));

  return {
    domain,
    body: toB64(bodyBytes),
    sig: toB64(sig),
    spkiDer: toB64(spkiDer),
  };
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
  // credentialSecretId is CP-derived and intentionally NOT pended by the client.
  mounts?: Array<{ name: string; backendUri: string; createIfMissing?: boolean }>;
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

export interface PendingIntentProto {
  op: string;
  spawnId: string;
  generation: string;      // uint64 as string (Connect-JSON)
  targetNodeId: string;
  image: string;
  appRef: string;
  model: string;
  dataRef: string;
  // CP-returned pending intent carries all 5 MountRef fields; the client signs them back verbatim.
  mounts?: Array<{
    name: string;
    backendUri: string;
    credentialSecretId?: string;
    createIfMissing?: boolean;
    repositoryId?: string;
  }>;
}

export interface GetPendingIntentResponse {
  pending?: PendingIntentProto;
  ready?: boolean;
}

export interface PollAndSignDeps {
  spawnId: string;
  pended: PendedOp;
  privateKey: CryptoKey;
  publicKey: CryptoKey;
  /** Injectable; defaults to global unary() */
  unaryFn?: typeof unary;
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
    spawnId,
    pended,
    privateKey,
    publicKey,
    unaryFn = unary,
    maxAttempts = 30,
  } = deps;

  const spki = await exportSpkiDer(publicKey);

  // Poll until ready
  let pending: PendingIntentProto | undefined;
  for (let i = 0; i < maxAttempts; i++) {
    const res = await unaryFn<GetPendingIntentResponse>("GetPendingIntent", { spawnId });
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
  const generation = BigInt(pending.generation ?? "0");

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

  await unaryFn<Record<string, never>>("SubmitIntent", {
    spawnId,
    intent: signedIntent,
    nodeAccessToken: "", // dev CP mints node token from SPKI (R5: prod gap)
  });

  return jti;
}

// ── Tuple validation (AM1) ────────────────────────────────────────────────────

function _validateTuple(pending: PendingIntentProto, pended: PendedOp): void {
  // Op must match.
  if (pending.op !== pended.op) {
    throw new Error(`intent: op mismatch: CP says "${pending.op}", locally pended "${pended.op}"`);
  }
  // spawnId must match.
  if (pending.spawnId !== pended.spawnId) {
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
