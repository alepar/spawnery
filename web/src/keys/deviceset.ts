/**
 * Device-set chain logic (Phase 3, [WM9][WM10][WM15]).
 *
 * Mirrors internal/secrets/seal/deviceset.go. The device-set is an append-only,
 * hash-chained, member-signed log. This module handles:
 *   - Types matching the Go JSON shapes
 *   - Chain verification (full chain + pinned head + fail-closed head-regression)
 *   - Entry building (genesis, add, remove) with ECDSA-P256 signing
 *   - DER↔P1363 signature interop
 *   - BigInt u64 nanosecond timestamp parsing (WM10)
 *   - CAS retry-rebase on 409 from the AS (WM1)
 *
 * WM9 canonical-bytes discipline: signatures and hashes are always computed
 * over the raw stored Body bytes — never over re-serialized JSON.
 */

import { encodeFields, sha256, fromBase64, toBase64, parseBigIntNanos, bytesEqual } from "./encoding";
// Note: sha256 is used in computeEntryHash (chain hash = sha256(encodeFields(...)))
import { derToP1363, p1363ToDer } from "./der";

// ── Types (matching Go JSON shapes) ──────────────────────────────────────────

export interface DeviceRef {
  x25519_pub: string; // base64 raw 32 bytes
  sign_pub: string; // base64 SEC1-uncompressed 65 bytes
}

export interface Signature {
  signer_pub: string; // base64 SEC1-uncompressed P-256 pubkey
  sig: string; // base64 ASN.1-DER ECDSA signature
}

export interface EntryLabel {
  name: string;
  enrolled_at: string; // decimal u64 UnixNano string (WM10)
}

/** StoredEntry is the on-the-wire shape of one log entry (WM9). */
export interface StoredEntry {
  body: string; // base64 JSON body bytes
  sigs: Signature[];
}

interface EntryBody {
  version: number;
  type: "genesis" | "add" | "remove";
  prev: string | null; // base64 SHA-256 of prior StoredEntry, null for genesis
  change: DeviceRef | null;
  devices: DeviceRef[];
  label?: EntryLabel; // WM15: authenticated label for the change device
  recovery_label?: EntryLabel; // WM15: genesis-only recovery device label
}

export interface OwnerRoot {
  device1_sign_pub: string; // base64
  recovery_sign_pub: string; // base64
}

export interface DeviceSetLog {
  entries: StoredEntry[];
}

export interface VerifyResult {
  members: DeviceRef[];
  headHash: Uint8Array;
  headVersion: number;
}

// ── Chain hash ────────────────────────────────────────────────────────────────

/**
 * computeEntryHash computes the chain-link hash for a StoredEntry.
 * Hash = SHA-256(encodeFields(Body, sig0.SignerPub, sig0.Sig, …))
 * This must match Go's StoredEntry.hash() exactly (WM9).
 */
export async function computeEntryHash(entry: StoredEntry): Promise<Uint8Array> {
  if (!entry.sigs || entry.sigs.length === 0) {
    throw new Error("deviceset: cannot hash an unsigned entry");
  }
  const parts: Uint8Array[] = [fromBase64(entry.body)];
  for (const s of entry.sigs) {
    parts.push(fromBase64(s.signer_pub));
    parts.push(fromBase64(s.sig));
  }
  return sha256(encodeFields(...parts));
}

// ── Chain verification ────────────────────────────────────────────────────────

/**
 * verifyDeviceSet replays and validates the full chain against ownerRoot.
 * Returns the verified membership and head hash, or throws on any violation.
 *
 * Rejections (fail-closed):
 *   - Genesis not co-signed by both owner roots
 *   - Entry version not strictly +1 (head regression = hard fail, [WM6])
 *   - Broken prev-hash link
 *   - Declared membership does not match add/remove delta
 *   - Mutation not signed by a current member
 *
 * Verification failure always throws — callers must never seal without a
 * verified chain. [WM6]: a stored head version higher than the fetched chain's
 * head is a hard failure.
 */
export async function verifyDeviceSet(
  log: DeviceSetLog,
  root: OwnerRoot,
  pinnedHeadVersion?: number, // WM6: reject if fetched head < pinned head
): Promise<VerifyResult> {
  if (!log.entries || log.entries.length === 0) {
    throw new Error("deviceset: empty log");
  }

  // [WM6] Head-regression hard fail: if we have a pinned head version, the
  // chain from the AS must not be shorter (withholding = stale-prefix attack).
  const fetchedHead = parseBody(log.entries[log.entries.length - 1]);
  if (pinnedHeadVersion !== undefined && fetchedHead.version < pinnedHeadVersion) {
    throw new Error(
      `deviceset: head regression — pinned version ${pinnedHeadVersion} but AS returned ${fetchedHead.version}`,
    );
  }

  // Genesis verification
  const genesis = log.entries[0];
  const genesisBody = parseBody(genesis);
  if (genesisBody.type !== "genesis") throw new Error("deviceset: first entry is not genesis");
  if (genesisBody.version !== 1) throw new Error("deviceset: genesis version must be 1");
  if (genesisBody.prev !== null) throw new Error("deviceset: genesis must have no prev-hash");

  await verifyGenesisSigs(genesis, root);

  let prev = genesis;
  let prevBody = genesisBody;
  let members: DeviceRef[] = [...genesisBody.devices];

  for (let i = 1; i < log.entries.length; i++) {
    const entry = log.entries[i];
    const body = parseBody(entry);

    // Monotonic version check (WM6: not just "not regressed" — must be exactly +1)
    if (body.version !== prevBody.version + 1) {
      throw new Error(
        `deviceset: entry ${i} version ${body.version} not monotonic (expected ${prevBody.version + 1})`,
      );
    }

    // Prev-hash link
    const prevHash = await computeEntryHash(prev);
    const entryPrevHash = body.prev ? fromBase64(body.prev) : new Uint8Array(0);
    if (!bytesEqual(prevHash, entryPrevHash)) {
      throw new Error(`deviceset: entry ${i} prev-hash mismatch (chain broken)`);
    }

    // Delta verification
    const expected = applyDelta(members, body);
    if (!sameSet(expected, body.devices)) {
      throw new Error(`deviceset: entry ${i} declared membership does not match its delta`);
    }

    // Member signature (WM9: over raw Body bytes)
    await verifyMemberSig(entry, members);

    members = expected;
    prev = entry;
    prevBody = body;
  }

  const headHash = await computeEntryHash(prev);
  return { members, headHash, headVersion: prevBody.version };
}

// ── Entry building ────────────────────────────────────────────────────────────

/** nowNanoStr returns the current Unix nanoseconds as a decimal string (WM10). */
export function nowNanoStr(): string {
  // Date.now() is milliseconds; multiply by 1_000_000 to get nanoseconds.
  // Use BigInt to preserve full u64 precision.
  return (BigInt(Date.now()) * 1_000_000n).toString();
}

/**
 * buildGenesisEntry creates the genesis StoredEntry co-signed by device1 and
 * recovery signing keys. enrolledAt defaults to nowNanoStr() if not provided
 * (pass a fixed value only for test vectors).
 */
export async function buildGenesisEntry(
  device1: DeviceRef,
  recovery: DeviceRef,
  device1Name: string,
  recoveryName: string,
  device1SignPriv: CryptoKey, // non-extractable ECDSA-P256 private key
  recoverySignPriv: CryptoKey,
  enrolledAt?: string,
): Promise<StoredEntry> {
  const at = enrolledAt ?? nowNanoStr();
  const body: EntryBody = {
    version: 1,
    type: "genesis",
    prev: null,
    change: null,
    devices: [device1, recovery],
    label: { name: device1Name, enrolled_at: at },
    recovery_label: { name: recoveryName, enrolled_at: at },
  };
  const bodyBytes = new TextEncoder().encode(JSON.stringify(body));
  const entry: StoredEntry = { body: toBase64(bodyBytes), sigs: [] };
  await signEntry(entry, device1SignPriv, device1.sign_pub);
  await signEntry(entry, recoverySignPriv, recovery.sign_pub);
  return entry;
}

/**
 * buildAddEntry creates a member-signed add entry enrolling a new device.
 * signer must be a current member.
 */
export async function buildAddEntry(
  log: DeviceSetLog,
  newDevice: DeviceRef,
  newDeviceName: string,
  signerRef: DeviceRef,
  signerPriv: CryptoKey,
  enrolledAt?: string,
): Promise<StoredEntry> {
  const prev = log.entries[log.entries.length - 1];
  if (!prev) throw new Error("deviceset: empty log");
  const prevBody = parseBody(prev);
  if (memberIndex(prevBody.devices, newDevice) >= 0) {
    throw new Error("deviceset: device already enrolled");
  }
  const prevHash = await computeEntryHash(prev);
  const devices = [...prevBody.devices, newDevice];
  const body: EntryBody = {
    version: prevBody.version + 1,
    type: "add",
    prev: toBase64(prevHash),
    change: newDevice,
    devices,
    label: { name: newDeviceName, enrolled_at: enrolledAt ?? nowNanoStr() },
  };
  const bodyBytes = new TextEncoder().encode(JSON.stringify(body));
  const entry: StoredEntry = { body: toBase64(bodyBytes), sigs: [] };
  await signEntry(entry, signerPriv, signerRef.sign_pub);
  return entry;
}

/**
 * buildRemoveEntry creates a member-signed remove entry.
 * signer must be a current member; targetX25519Pub identifies the device to remove.
 */
export async function buildRemoveEntry(
  log: DeviceSetLog,
  targetX25519Pub: string, // base64 raw 32 bytes
  signerRef: DeviceRef,
  signerPriv: CryptoKey,
): Promise<StoredEntry> {
  const prev = log.entries[log.entries.length - 1];
  if (!prev) throw new Error("deviceset: empty log");
  const prevBody = parseBody(prev);
  const idx = prevBody.devices.findIndex((d) => d.x25519_pub === targetX25519Pub);
  if (idx < 0) throw new Error("deviceset: device not enrolled");
  const removed = prevBody.devices[idx];
  const devices = prevBody.devices.filter((_, i) => i !== idx);
  const prevHash = await computeEntryHash(prev);
  const body: EntryBody = {
    version: prevBody.version + 1,
    type: "remove",
    prev: toBase64(prevHash),
    change: removed,
    devices,
  };
  const bodyBytes = new TextEncoder().encode(JSON.stringify(body));
  const entry: StoredEntry = { body: toBase64(bodyBytes), sigs: [] };
  await signEntry(entry, signerPriv, signerRef.sign_pub);
  return entry;
}

// ── AS CAS append with retry-rebase (WM1) ────────────────────────────────────

export interface AppendResult {
  version: number;
  head: string; // base64
}

export interface ConflictInfo {
  head: string; // base64 current head from AS
  version: number;
}

/**
 * appendEntry posts a single entry to the AS device-set registry.
 * Returns the new head on success, or throws with ConflictError on 409.
 *
 * The AS is a pure head-comparison CAS (WM1): it never validates signatures —
 * it just checks PrevHash ≡ stored head before appending.
 */
export async function appendEntry(
  asUrl: string,
  bearerToken: string,
  entry: StoredEntry,
): Promise<AppendResult> {
  const entryBytes = new TextEncoder().encode(JSON.stringify(entry));
  const resp = await fetch(asUrl + "/devices/append", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: "Bearer " + bearerToken,
    },
    body: JSON.stringify({ entry: toBase64(entryBytes) }),
  });
  if (resp.status === 409) {
    const body = await resp.json();
    const err = new ConflictError(body.head as string, body.version as number);
    throw err;
  }
  if (!resp.ok) {
    const text = await resp.text().catch(() => resp.statusText);
    throw new Error(`deviceset: append failed ${resp.status}: ${text}`);
  }
  return resp.json();
}

/** ConflictError is thrown by appendEntry on a 409 CAS conflict. */
export class ConflictError extends Error {
  readonly currentHead: string;
  readonly currentVersion: number;
  constructor(currentHead: string, currentVersion: number) {
    super(`deviceset: CAS conflict — current head version ${currentVersion}`);
    this.name = "ConflictError";
    this.currentHead = currentHead;
    this.currentVersion = currentVersion;
  }
}

/**
 * fetchDeviceSetLog fetches the full chain from the AS.
 */
export async function fetchDeviceSetLog(
  asUrl: string,
  bearerToken: string,
): Promise<{ log: DeviceSetLog; head: string; version: number }> {
  const resp = await fetch(asUrl + "/devices", {
    headers: { Authorization: "Bearer " + bearerToken },
  });
  if (!resp.ok) {
    throw new Error(`deviceset: fetch failed ${resp.status}`);
  }
  const body = await resp.json();
  const entries: StoredEntry[] = (body.entries as string[]).map((b64: string) => {
    const raw = fromBase64(b64);
    return JSON.parse(new TextDecoder().decode(raw)) as StoredEntry;
  });
  return {
    log: { entries },
    head: body.head as string,
    version: body.version as number,
  };
}

// ── Internal helpers ──────────────────────────────────────────────────────────

function parseBody(entry: StoredEntry): EntryBody {
  const raw = fromBase64(entry.body);
  return JSON.parse(new TextDecoder().decode(raw)) as EntryBody;
}

async function verifyGenesisSigs(entry: StoredEntry, root: OwnerRoot): Promise<void> {
  // WM9: verify over raw Body bytes. WebCrypto ECDSA verify takes the raw
  // message (not a pre-computed hash) and computes SHA-256 internally —
  // matching Go's ecdsa.SignASN1(key, sha256(body)).
  const bodyBytes = fromBase64(entry.body);
  let haveDev1 = false;
  let haveRec = false;
  for (const s of entry.sigs) {
    if (s.signer_pub === root.device1_sign_pub) {
      if (await verifySig(root.device1_sign_pub, bodyBytes, s.sig)) haveDev1 = true;
    }
    if (s.signer_pub === root.recovery_sign_pub) {
      if (await verifySig(root.recovery_sign_pub, bodyBytes, s.sig)) haveRec = true;
    }
  }
  if (!haveDev1 || !haveRec) {
    throw new Error("deviceset: genesis not co-signed by device1 + recovery owner roots");
  }
}

async function verifyMemberSig(entry: StoredEntry, members: DeviceRef[]): Promise<void> {
  // WM9: verify over raw Body bytes (see verifyGenesisSigs comment).
  const bodyBytes = fromBase64(entry.body);
  for (const s of entry.sigs) {
    if (!memberHasSignPub(members, s.signer_pub)) continue;
    if (await verifySig(s.signer_pub, bodyBytes, s.sig)) return;
  }
  throw new Error("deviceset: not signed by a current member");
}

/**
 * verifySig verifies an ASN.1-DER ECDSA signature over raw body bytes.
 *
 * The signature in the chain is produced by Go as:
 *   ecdsa.SignASN1(rand.Reader, key, sha256(body))
 *
 * WebCrypto's verify({name:"ECDSA", hash:"SHA-256"}, key, sig, data)
 * computes sha256(data) internally and then verifies — so we pass raw bodyBytes,
 * not a pre-computed digest (which would cause double-hashing).
 */
async function verifySig(
  signerPubB64: string,
  bodyBytes: Uint8Array, // raw body bytes, NOT pre-hashed
  sigB64: string,
): Promise<boolean> {
  try {
    const signerPub = fromBase64(signerPubB64);
    const derSig = fromBase64(sigB64);
    const p1363 = derToP1363(derSig);
    const pubKey = await importP256PublicKey(signerPub);
    // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
    return crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" },
      pubKey,
      p1363 as unknown as Uint8Array<ArrayBuffer>,
      bodyBytes as unknown as Uint8Array<ArrayBuffer>,
    );
  } catch {
    return false;
  }
}

/** signEntry appends a P-256 DER signature over sha256(body) (WM9). */
async function signEntry(
  entry: StoredEntry,
  signerPriv: CryptoKey,
  signerPubB64: string,
): Promise<void> {
  const bodyBytes = fromBase64(entry.body);
  // WebCrypto ECDSA with {hash:"SHA-256"} computes the digest internally over bodyBytes.
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const p1363 = new Uint8Array(
    await crypto.subtle.sign(
      { name: "ECDSA", hash: "SHA-256" },
      signerPriv,
      bodyBytes as unknown as Uint8Array<ArrayBuffer>,
    ),
  );
  const der = p1363ToDer(p1363);
  entry.sigs.push({
    signer_pub: signerPubB64,
    sig: toBase64(der),
  });
}

async function importP256PublicKey(sec1Uncompressed: Uint8Array): Promise<CryptoKey> {
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  return crypto.subtle.importKey(
    "raw",
    sec1Uncompressed as unknown as Uint8Array<ArrayBuffer>,
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
}

function applyDelta(members: DeviceRef[], body: EntryBody): DeviceRef[] {
  switch (body.type) {
    case "add": {
      if (!body.change) throw new Error("deviceset: add entry missing change");
      if (memberIndex(members, body.change) >= 0)
        throw new Error("deviceset: add of already-enrolled device");
      return [...members, body.change];
    }
    case "remove": {
      if (!body.change) throw new Error("deviceset: remove entry missing change");
      const idx = memberIndex(members, body.change);
      if (idx < 0) throw new Error("deviceset: remove of non-member device");
      return members.filter((_, i) => i !== idx);
    }
    default:
      throw new Error(`deviceset: unexpected entry type ${body.type}`);
  }
}

function memberIndex(set: DeviceRef[], d: DeviceRef): number {
  return set.findIndex(
    (m) => m.x25519_pub === d.x25519_pub && m.sign_pub === d.sign_pub,
  );
}

function memberHasSignPub(set: DeviceRef[], signPub: string): boolean {
  return set.some((m) => m.sign_pub === signPub);
}

function sameSet(a: DeviceRef[], b: DeviceRef[]): boolean {
  if (a.length !== b.length) return false;
  return a.every((x) => memberIndex(b, x) >= 0);
}

// Re-export for consumers that need the BigInt nanos parser
export { parseBigIntNanos };
