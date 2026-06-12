/**
 * HPKE Base mode recipient-side envelope operations (Phase 3, [M15][WM9]).
 *
 * Implements DHKEM(X25519, HKDF-SHA256) + HKDF-SHA256 + AES-256-GCM (RFC 9180)
 * matching Go's internal/secrets/seal/seal.go using only native WebCrypto.
 *
 * The recipient-side DH uses:
 *   crypto.subtle.deriveBits({name:'X25519', public: enc}, devicePrivKey, 256)
 * on the non-extractable device CryptoKey, preserving the extractable:false
 * invariant (spec §1, [roast M15]). A pure-JS noble polyfill would force
 * extractable device keys — noble is only used for ephemeral/sender ops.
 *
 * The sender side (for re-sealing) derives an ephemeral key via generateKey,
 * which is ephemeral-only and therefore OK to keep extractable.
 *
 * Suite: DHKEM-X25519-HKDF-SHA256 (KEM id 0x0020) + HKDF-SHA256 (KDF id 0x0001)
 *        + AES-256-GCM (AEAD id 0x0002)
 *
 * RFC 9180 §A.1 test vector is validated in hpke.test.ts.
 */

// Suite and KEM identifiers (RFC 9180 §5.1 and §7.1)
const HPKE_SUITE_ID = new Uint8Array([
  0x48, 0x50, 0x4b, 0x45, // "HPKE"
  0x00, 0x20, // KEM id: DHKEM-X25519-HKDF-SHA256
  0x00, 0x01, // KDF id: HKDF-SHA256
  0x00, 0x02, // AEAD id: AES-256-GCM
]);

const KEM_SUITE_ID = new Uint8Array([
  0x4b, 0x45, 0x4d, // "KEM"
  0x00, 0x20, // KEM id: DHKEM-X25519-HKDF-SHA256
]);

const HPKE_VERSION = new TextEncoder().encode("HPKE-v1");

// ── HPKE types matching Go's seal.go ─────────────────────────────────────────

export interface RecipientSeal {
  recipient: string; // base64 raw 32-byte X25519 pubkey (hint only)
  enc: string; // base64 HPKE encapsulated key (32 bytes for X25519)
  ct: string; // base64 sealed DEK
}

export interface AtRestAAD {
  account_id: string;
  secret_id: string;
  version: number;
}

export interface Envelope {
  at_rest: AtRestAAD;
  recipients: RecipientSeal[];
  nonce: string; // base64 AES-256-GCM nonce (12 bytes)
  ct: string; // base64 payload ciphertext
}

// ── AAD encoding ──────────────────────────────────────────────────────────────

function encodeFields(...parts: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const p of parts) total += 8 + p.length;
  const out = new Uint8Array(total);
  let off = 0;
  const view = new DataView(out.buffer);
  for (const p of parts) {
    view.setBigUint64(off, BigInt(p.length), false);
    off += 8;
    out.set(p, off);
    off += p.length;
  }
  return out;
}

function u64(v: number): Uint8Array {
  const b = new Uint8Array(8);
  new DataView(b.buffer).setBigUint64(0, BigInt(v), false);
  return b;
}

function atRestAADBytes(aad: AtRestAAD): Uint8Array {
  const enc = new TextEncoder();
  return encodeFields(
    enc.encode("at-rest/v1"),
    enc.encode(aad.account_id),
    enc.encode(aad.secret_id),
    u64(aad.version),
  );
}

function fromBase64(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function toBase64(b: Uint8Array): string {
  let s = "";
  for (const byte of b) s += String.fromCharCode(byte);
  return btoa(s);
}

function concat(...arrays: Uint8Array[]): Uint8Array {
  const total = arrays.reduce((n, a) => n + a.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const a of arrays) { out.set(a, off); off += a.length; }
  return out;
}

function i2osp2(n: number): Uint8Array {
  return new Uint8Array([n >> 8, n & 0xff]);
}

// ── RFC 9180 LabeledExtract / LabeledExpand ───────────────────────────────────

async function labeledExtract(
  suiteId: Uint8Array,
  salt: Uint8Array | null,
  label: string,
  ikm: Uint8Array,
): Promise<Uint8Array> {
  const enc = new TextEncoder();
  const labeledIKM = concat(HPKE_VERSION, suiteId, enc.encode(label), ikm);
  const saltKey = salt && salt.length > 0 ? salt : new Uint8Array(32); // RFC 5869: zero-filled if empty
  // TS 5.9: Uint8Array<ArrayBufferLike> not assignable to BufferSource; cast is safe.
  const hmacKey = await crypto.subtle.importKey(
    "raw", saltKey as unknown as Uint8Array<ArrayBuffer>, { name: "HMAC", hash: "SHA-256" }, false, ["sign"],
  );
  const prk = new Uint8Array(await crypto.subtle.sign("HMAC", hmacKey, labeledIKM as unknown as Uint8Array<ArrayBuffer>));
  return prk;
}

async function labeledExpand(
  suiteId: Uint8Array,
  prk: Uint8Array,
  label: string,
  info: Uint8Array,
  length: number,
): Promise<Uint8Array> {
  const enc = new TextEncoder();
  const labeledInfo = concat(
    i2osp2(length),
    HPKE_VERSION,
    suiteId,
    enc.encode(label),
    info,
  );
  // HKDF-Expand with prk as the key
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const prkKey = await crypto.subtle.importKey(
    "raw", prk as unknown as Uint8Array<ArrayBuffer>, { name: "HMAC", hash: "SHA-256" }, false, ["sign"],
  );
  const blocks = Math.ceil(length / 32);
  let prev = new Uint8Array(0);
  const output = new Uint8Array(length);
  let outputOff = 0;
  for (let i = 1; i <= blocks; i++) {
    const data = concat(prev, labeledInfo, new Uint8Array([i]));
    prev = new Uint8Array(await crypto.subtle.sign("HMAC", prkKey, data as unknown as Uint8Array<ArrayBuffer>));
    const take = Math.min(32, length - outputOff);
    output.set(prev.subarray(0, take), outputOff);
    outputOff += take;
  }
  return output;
}

// ── DHKEM DeriveKeyPair (RFC 9180 §7.1.2) ────────────────────────────────────

/**
 * dhkemDerivePrivateScalar implements DHKEM(X25519, HKDF-SHA256) DeriveKeyPair
 * (RFC 9180 §7.1.2), returning the raw 32-byte X25519 private scalar derived
 * from ikm. Matches Go: kemScheme.DeriveKeyPair(ikm) → sk.MarshalBinary()
 * (github.com/cloudflare/circl/hpke, xKEM.DeriveKeyPair).
 *
 * Algorithm:
 *   dkp_prk = LabeledExtract("", "dkp_prk", ikm)   // HKDF-Extract with KEM suite-id
 *   scalar  = LabeledExpand(dkp_prk, "sk", "", 32) // HKDF-Expand with KEM suite-id
 */
export async function dhkemDerivePrivateScalar(ikm: Uint8Array): Promise<Uint8Array> {
  const dkpPrk = await labeledExtract(KEM_SUITE_ID, null, "dkp_prk", ikm);
  return labeledExpand(KEM_SUITE_ID, dkpPrk, "sk", new Uint8Array(0), 32);
}

// ── KEM ExtractAndExpand (RFC 9180 §7.1) ─────────────────────────────────────

/**
 * kemExtractAndExpand implements DHKEM-X25519-HKDF-SHA256 ExtractAndExpand
 * (RFC 9180 §7.1.4 as implemented by CIRCL / Go's cloudflare/circl/hpke):
 *   eae_prk = LabeledExtract("", "eae_prk", dh)
 *   shared_secret = LabeledExpand(eae_prk, "shared_secret", kem_context, 32)
 *
 * Note: CIRCL (which Go uses) extracts under the "eae_prk" label. The
 * intermediate draft that CIRCL tracks uses this label; using "shared_secret"
 * for the extract step produces a different shared secret and breaks
 * interoperability with Go's seal.go.
 */
async function kemExtractAndExpand(
  dh: Uint8Array,
  kemContext: Uint8Array,
): Promise<Uint8Array> {
  const prk = await labeledExtract(KEM_SUITE_ID, null, "eae_prk", dh);
  return labeledExpand(KEM_SUITE_ID, prk, "shared_secret", kemContext, 32);
}

/** decap performs the DHKEM-X25519 recipient Decap operation (RFC 9180 §7.1). */
async function decap(
  encBytes: Uint8Array, // sender's ephemeral X25519 pubkey (32 bytes)
  recipPriv: CryptoKey, // non-extractable X25519 private key
  recipPubBytes: Uint8Array, // recipient's own X25519 public key (32 bytes)
): Promise<Uint8Array> {
  // Import the sender's ephemeral public key
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const encKey = await crypto.subtle.importKey(
    "raw",
    encBytes as unknown as Uint8Array<ArrayBuffer>,
    { name: "X25519" },
    false,
    [],
  );
  // DH via non-extractable key — this is the critical deriveBits call (spec §1)
  const dhBits = await crypto.subtle.deriveBits(
    { name: "X25519", public: encKey } as AlgorithmIdentifier,
    recipPriv,
    256,
  );
  const dh = new Uint8Array(dhBits);
  // kem_context = enc || pk(recip)
  const kemContext = concat(encBytes, recipPubBytes);
  return kemExtractAndExpand(dh, kemContext);
}

// ── HPKE KeySchedule (Base mode, RFC 9180 §5.1) ──────────────────────────────

interface KeyScheduleResult {
  key: CryptoKey; // AES-256-GCM key
  baseNonce: Uint8Array; // 12-byte base nonce
}

async function keyScheduleBase(
  sharedSecret: Uint8Array,
  info: Uint8Array,
): Promise<KeyScheduleResult> {
  // Base mode: psk = "", psk_id = ""
  const pskIdHash = await labeledExtract(
    HPKE_SUITE_ID, null, "psk_id_hash", new Uint8Array(0),
  );
  const infoHash = await labeledExtract(
    HPKE_SUITE_ID, null, "info_hash", info,
  );
  // ks_context = mode(0) || psk_id_hash || info_hash
  const ksContext = concat(new Uint8Array([0]), pskIdHash, infoHash);

  const prk = await labeledExtract(
    HPKE_SUITE_ID,
    sharedSecret,
    "secret",
    new Uint8Array(0), // psk = "" in base mode
  );

  // key (32 bytes for AES-256-GCM)
  const keyBytes = await labeledExpand(HPKE_SUITE_ID, prk, "key", ksContext, 32);
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const key = await crypto.subtle.importKey(
    "raw", keyBytes as unknown as Uint8Array<ArrayBuffer>, { name: "AES-GCM" }, false, ["decrypt", "encrypt"],
  );

  // base_nonce (12 bytes for AES-256-GCM)
  const baseNonce = await labeledExpand(HPKE_SUITE_ID, prk, "base_nonce", ksContext, 12);

  return { key, baseNonce };
}

// ── Recipient Open (decapsulate DEK) ─────────────────────────────────────────

/**
 * hpkeOpen opens a single HPKE RecipientSeal to recover the DEK.
 * Uses the non-extractable X25519 private key via deriveBits.
 *
 * info = "spawnery/secrets/seal/at-rest/v1" (infoAtRest from seal.go)
 */
export async function hpkeOpen(
  rs: RecipientSeal,
  recipPriv: CryptoKey,
  recipPubBytes: Uint8Array,
  aad: Uint8Array,
  info: string = "spawnery/secrets/seal/at-rest/v1",
): Promise<Uint8Array> {
  const encBytes = fromBase64(rs.enc);
  const sharedSecret = await decap(encBytes, recipPriv, recipPubBytes);
  const { key, baseNonce } = await keyScheduleBase(
    sharedSecret,
    new TextEncoder().encode(info),
  );
  // Single-message Open (seq=0): nonce = base_nonce XOR 0 = base_nonce
  const ciphertext = fromBase64(rs.ct);
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: baseNonce as unknown as Uint8Array<ArrayBuffer>, additionalData: aad as unknown as Uint8Array<ArrayBuffer> },
    key,
    ciphertext as unknown as Uint8Array<ArrayBuffer>,
  );
  return new Uint8Array(plaintext);
}

// ── Sender Encap ─────────────────────────────────────────────────────────────

/**
 * encap performs the DHKEM-X25519 sender Encap operation (RFC 9180 §7.1).
 * Generates a fresh ephemeral X25519 keypair (extractable is fine here — the
 * key is single-use and never stored as a device key), computes DH against the
 * recipient, and returns enc (ephemeral pubkey) + shared_secret.
 */
async function encap(
  recipPubBytes: Uint8Array,
): Promise<{ enc: Uint8Array; sharedSecret: Uint8Array }> {
  // Ephemeral key: extractable so we can export the public bytes (enc)
  // TS 5.9: cast required — generateKey returns CryptoKeyPair for asymmetric algos.
  const ephemeral = await crypto.subtle.generateKey(
    { name: "X25519" },
    true, // extractable: needed to export enc (ephemeral pub); never persisted
    ["deriveBits"],
  ) as CryptoKeyPair;

  // enc = raw ephemeral public key (32 bytes)
  const encBuf = await crypto.subtle.exportKey("raw", ephemeral.publicKey);
  const enc = new Uint8Array(encBuf as ArrayBuffer);

  // Import recipient public key for DH
  const recipKey = await crypto.subtle.importKey(
    "raw",
    recipPubBytes as unknown as Uint8Array<ArrayBuffer>,
    { name: "X25519" },
    false,
    [],
  );

  // DH: ephemeralPriv × recipPub
  const dhBits = await crypto.subtle.deriveBits(
    { name: "X25519", public: recipKey } as AlgorithmIdentifier,
    ephemeral.privateKey,
    256,
  );
  const dh = new Uint8Array(dhBits);

  // kem_context = enc || pk(recip)
  const kemContext = concat(enc, recipPubBytes);
  const sharedSecret = await kemExtractAndExpand(dh, kemContext);
  return { enc, sharedSecret };
}

/**
 * hpkeSeal seals a DEK to one recipient's X25519 public key using HPKE Base
 * mode (RFC 9180). Matches Go's suite.NewSender + Setup + Seal leg in seal.go.
 *
 * info = "spawnery/secrets/seal/at-rest/v1" (infoAtRest from seal.go)
 */
export async function hpkeSeal(
  dek: Uint8Array,
  recipPubBytes: Uint8Array,
  aad: Uint8Array,
  info: string = "spawnery/secrets/seal/at-rest/v1",
): Promise<RecipientSeal> {
  const { enc, sharedSecret } = await encap(recipPubBytes);
  const { key, baseNonce } = await keyScheduleBase(
    sharedSecret,
    new TextEncoder().encode(info),
  );
  // Single-message Seal (seq=0): nonce = base_nonce XOR 0 = base_nonce
  // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
  const ct = new Uint8Array(await crypto.subtle.encrypt(
    {
      name: "AES-GCM",
      iv: baseNonce as unknown as Uint8Array<ArrayBuffer>,
      additionalData: aad as unknown as Uint8Array<ArrayBuffer>,
    },
    key,
    dek as unknown as Uint8Array<ArrayBuffer>,
  ));
  return {
    recipient: toBase64(recipPubBytes),
    enc: toBase64(enc),
    ct: toBase64(ct),
  };
}

/**
 * sealEnvelope encrypts payload under a fresh random DEK (new DEK on every
 * call — roast M2) and HPKE-Base-seals that DEK to each recipient pubkey,
 * binding atRest into every seal. Matches Go's seal.Seal() exactly.
 */
export async function sealEnvelope(
  payload: Uint8Array,
  recipients: Uint8Array[], // raw 32-byte X25519 pubkeys
  atRest: AtRestAAD,
): Promise<Envelope> {
  if (recipients.length === 0) throw new Error("hpke: no recipients");
  const aad = atRestAADBytes(atRest);

  // Fresh DEK per write [roast M2]
  const dek = crypto.getRandomValues(new Uint8Array(32));
  try {
    // Encrypt payload under the DEK (AES-256-GCM), binding atRest as AAD
    // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
    const gcmKey = await crypto.subtle.importKey(
      "raw", dek as unknown as Uint8Array<ArrayBuffer>, { name: "AES-GCM" }, false, ["encrypt"],
    );
    const nonce = crypto.getRandomValues(new Uint8Array(12));
    const ctBytes = new Uint8Array(await crypto.subtle.encrypt(
      {
        name: "AES-GCM",
        iv: nonce as unknown as Uint8Array<ArrayBuffer>,
        additionalData: aad as unknown as Uint8Array<ArrayBuffer>,
      },
      gcmKey,
      payload as unknown as Uint8Array<ArrayBuffer>,
    ));

    // HPKE-seal the DEK to each recipient
    const recipientSeals = await Promise.all(
      recipients.map((r) => hpkeSeal(dek, r, aad)),
    );

    return {
      at_rest: atRest,
      recipients: recipientSeals,
      nonce: toBase64(nonce),
      ct: toBase64(ctBytes),
    };
  } finally {
    dek.fill(0); // best-effort DEK zeroization
  }
}

/**
 * reSealEnvelope opens an existing envelope with this device's key, then
 * re-seals the plaintext to a new recipient set with a fresh DEK (roast M2)
 * and a bumped version. Used by the re-seal sweep after enrollment or removal.
 */
export async function reSealEnvelope(
  env: Envelope,
  recipPriv: CryptoKey,
  recipPubBytes: Uint8Array,
  newRecipients: Uint8Array[],
): Promise<Envelope> {
  // Open the existing envelope to recover the plaintext payload
  const plaintext = await openEnvelope(env, recipPriv, recipPubBytes);

  // Re-seal with a fresh DEK to the new member set; bump version to prevent
  // the CP from replaying the pre-removal ciphertext (§2, AAD splice defence).
  const newAtRest: AtRestAAD = {
    ...env.at_rest,
    version: env.at_rest.version + 1,
  };
  return sealEnvelope(plaintext, newRecipients, newAtRest);
}

// ── Envelope Open ─────────────────────────────────────────────────────────────

/**
 * openEnvelope recovers the payload from an at-rest envelope using one
 * device's X25519 private key (trial-open across recipient stanzas).
 */
export async function openEnvelope(
  env: Envelope,
  recipPriv: CryptoKey,
  recipPubBytes: Uint8Array,
): Promise<Uint8Array> {
  const aad = atRestAADBytes(env.at_rest);

  // Trial-open each stanza (rotation-robust)
  for (const rs of env.recipients) {
    try {
      const dek = await hpkeOpen(rs, recipPriv, recipPubBytes, aad);
      if (dek.length !== 32) continue;

      // Decrypt payload with the recovered DEK
      // TS 5.9: cast Uint8Array<ArrayBufferLike> to concrete buffer type for WebCrypto.
      const gcmKey = await crypto.subtle.importKey(
        "raw", dek as unknown as Uint8Array<ArrayBuffer>, { name: "AES-GCM" }, false, ["decrypt"],
      );
      const nonce = fromBase64(env.nonce);
      const ct = fromBase64(env.ct);
      const plaintext = await crypto.subtle.decrypt(
        { name: "AES-GCM", iv: nonce as unknown as Uint8Array<ArrayBuffer>, additionalData: aad as unknown as Uint8Array<ArrayBuffer> },
        gcmKey,
        ct as unknown as Uint8Array<ArrayBuffer>,
      );
      return new Uint8Array(plaintext);
    } catch {
      continue; // try next stanza
    }
  }
  throw new Error("hpke: device key is not a recipient of this envelope");
}
