/**
 * Narrowed X.509 DER parser for Spawnery's P-256 node cert profile.
 *
 * Parses only the fields we need:
 *   - TBSCertificate bytes (signed portion, for chain verification)
 *   - Signature bytes (ECDSA-ASN.1, converted to P1363 for SubtleCrypto)
 *   - SubjectPublicKeyInfo (raw SPKI bytes for SubtleCrypto.importKey)
 *   - SubjectAltName DNS name (node identity extraction)
 *
 * Our node cert profile:
 *   - ECDSA-SHA256 signature algorithm
 *   - P-256 (secp256r1) EC key
 *   - DNS SAN = <nodeId>.<accountId>.<class>.nodes.spawnery.internal
 *
 * Design: docs/superpowers/specs/2026-06-10-owner-sealed-secrets-design.md §1
 */

import { derToP1363 } from "./der";

// ── ASN.1 tag constants ───────────────────────────────────────────────────────

const TAG_SEQUENCE  = 0x30;
const TAG_BIT_STR   = 0x03;
const TAG_OID       = 0x06;
const TAG_OCTET_STR = 0x04;
const TAG_CONTEXT_3 = 0xa3; // [3] EXPLICIT Extensions
const TAG_CONTEXT_2 = 0x82; // [2] dNSName in GeneralName CHOICE

// ── DER TLV reader ────────────────────────────────────────────────────────────

interface TLV {
  tag: number;
  /** Offset of the first byte of this TLV (tag byte) in the parent buffer. */
  startOff: number;
  /** The value bytes. */
  val: Uint8Array;
  /** Offset just past the end of this TLV (first byte of the next sibling). */
  next: number;
}

/** Read a single DER TLV at offset off in buf. */
function readTLV(buf: Uint8Array, off: number): TLV {
  if (off >= buf.length) throw new Error("x509: DER truncated");
  const startOff = off;
  const tag = buf[off++];
  if (off >= buf.length) throw new Error("x509: DER truncated at length");
  let len = buf[off++];
  if (len & 0x80) {
    const nb = len & 0x7f;
    if (nb === 0 || nb > 4) throw new Error("x509: DER indefinite or >4-byte length not supported");
    len = 0;
    for (let i = 0; i < nb; i++) {
      if (off >= buf.length) throw new Error("x509: DER truncated in length");
      len = (len << 8) | buf[off++];
    }
  }
  if (off + len > buf.length) throw new Error("x509: DER value extends past buffer");
  return { tag, startOff, val: buf.subarray(off, off + len), next: off + len };
}

/** Iterate over child TLVs within a constructed (SEQUENCE/SET) value. */
function* iterSeq(buf: Uint8Array): IterableIterator<TLV> {
  let off = 0;
  while (off < buf.length) {
    const t = readTLV(buf, off);
    yield t;
    off = t.next;
  }
}

// ── PEM helpers ───────────────────────────────────────────────────────────────

/** Parse one or more PEM blocks, returning their DER (binary) contents in order. */
export function pemToDerList(pem: string): Uint8Array[] {
  const out: Uint8Array[] = [];
  const re = /-----BEGIN [^-]+-----\r?\n([\s\S]+?)\r?\n-----END [^-]+-----/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(pem)) !== null) {
    const b64 = m[1].replace(/\s+/g, "");
    const bin = atob(b64);
    const der = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) der[i] = bin.charCodeAt(i);
    out.push(der);
  }
  return out;
}

// ── X.509 cert parsing ────────────────────────────────────────────────────────

/** Parsed fields from a single X.509 certificate. */
export interface ParsedCert {
  /** Raw DER bytes of the TBSCertificate TLV (what is signed by the issuer). */
  tbsBytes: Uint8Array;
  /** ECDSA signature bytes in ASN.1 DER format (strip unused-bits prefix). */
  sigDER: Uint8Array;
  /** SubjectPublicKeyInfo TLV bytes (for SubtleCrypto.importKey("spki", ...)). */
  spkiBytes: Uint8Array;
  /** First DNS SAN, if present; empty string otherwise. */
  sanDNS: string;
}

/**
 * Parse a DER-encoded X.509 certificate.
 *
 * Certificate ::= SEQUENCE {
 *   tbsCertificate TBSCertificate,
 *   signatureAlgorithm AlgorithmIdentifier,
 *   signature BIT STRING
 * }
 */
export function parseCertDER(der: Uint8Array): ParsedCert {
  const outer = readTLV(der, 0);
  if (outer.tag !== TAG_SEQUENCE) throw new Error("x509: certificate is not a SEQUENCE");

  const children = [...iterSeq(outer.val)];
  if (children.length < 3) throw new Error("x509: certificate has < 3 elements");

  const tbsChild = children[0];
  const sigChild = children[2];

  if (tbsChild.tag !== TAG_SEQUENCE) throw new Error("x509: TBSCertificate is not a SEQUENCE");
  if (sigChild.tag !== TAG_BIT_STR)  throw new Error("x509: cert signature is not a BIT STRING");

  // TBSCertificate TLV bytes: startOff within outer.val, so we reconstruct within der.
  // outer.val starts at offset (outer header length) in der.
  const outerHdrLen = tbsChild.startOff + (der.byteOffset - outer.val.byteOffset + outer.val.byteOffset - outer.val.byteOffset);
  // Simpler: the TBSCertificate full TLV starts at (outer tag+len offset + tbsChild.startOff).
  // We compute the outer header length = der.length - outer.val.length.
  const outerHeaderLen = der.length - outer.val.length;
  const tbsStart = outerHeaderLen + tbsChild.startOff;
  const tbsEnd   = outerHeaderLen + tbsChild.next;
  const tbsBytes = der.subarray(tbsStart, tbsEnd);

  // BIT STRING: first byte = unused-bit count; must be 0 for ECDSA-P256.
  if (sigChild.val[0] !== 0) throw new Error("x509: signature BIT STRING has non-zero unused bits");
  const sigDER = sigChild.val.subarray(1);

  const { spkiBytes, sanDNS } = parseTBS(tbsChild.val, outer.val);

  return { tbsBytes, sigDER, spkiBytes, sanDNS };
}

interface TBSFields {
  spkiBytes: Uint8Array;
  sanDNS: string;
}

/**
 * Parse TBSCertificate body for SPKI and SAN.
 *
 * TBSCertificate contains (in RFC 5280 order):
 *   [0] version (optional), serialNumber, signature, issuer, validity, subject,
 *   SubjectPublicKeyInfo, [3] extensions (optional)
 *
 * tbsVal is the VALUE bytes of the TBSCertificate; certVal is the value of the outer
 * Certificate SEQUENCE (used to get absolute offsets back into the cert for SPKI).
 */
function parseTBS(tbsVal: Uint8Array, certVal: Uint8Array): TBSFields {
  const items = [...iterSeq(tbsVal)];
  if (items.length < 6) throw new Error("x509: TBSCertificate too short");

  // Find [3] EXPLICIT extensions (tag 0xa3) if present.
  let extsIdx = -1;
  for (let i = 0; i < items.length; i++) {
    if (items[i].tag === TAG_CONTEXT_3) { extsIdx = i; break; }
  }

  // SubjectPublicKeyInfo = last SEQUENCE before [3] extensions (or last item if no exts).
  const spkiIdx = extsIdx >= 0 ? extsIdx - 1 : items.length - 1;
  const spkiItem = items[spkiIdx];
  if (spkiItem.tag !== TAG_SEQUENCE) throw new Error("x509: SPKI is not a SEQUENCE");

  // We need the full SPKI TLV (tag+len+val) from tbsVal.
  const spkiBytes = tbsVal.subarray(spkiItem.startOff, spkiItem.next);

  // Parse SAN if extensions are present.
  let sanDNS = "";
  if (extsIdx >= 0) {
    sanDNS = parseSAN(items[extsIdx].val);
  }

  void certVal; // certVal not needed for computing SPKI offset within tbsVal
  return { spkiBytes, sanDNS };
}

/** SAN OID 2.5.29.17 in DER: 55 1d 11. */
const SAN_OID = new Uint8Array([0x55, 0x1d, 0x11]);

/**
 * Parse the [3] Extensions body for the SubjectAltName DNS name.
 *
 * [3] ::= SEQUENCE OF Extension
 * Extension ::= SEQUENCE { OID, [critical BOOLEAN,] OCTET STRING(GeneralNames) }
 * GeneralNames ::= SEQUENCE OF GeneralName
 * GeneralName ::= CHOICE { dNSName [2] IA5String, ... }
 */
function parseSAN(extsBody: Uint8Array): string {
  // extsBody is the value of the [3] EXPLICIT wrapper; it contains a SEQUENCE of Extensions.
  const extsList = readTLV(extsBody, 0);
  if (extsList.tag !== TAG_SEQUENCE) throw new Error("x509: extensions outer is not a SEQUENCE");

  for (const ext of iterSeq(extsList.val)) {
    if (ext.tag !== TAG_SEQUENCE) continue;
    const extChildren = [...iterSeq(ext.val)];
    if (extChildren.length < 2) continue;
    if (extChildren[0].tag !== TAG_OID) continue;
    if (!bytesEqual(extChildren[0].val, SAN_OID)) continue;
    // Last element is OCTET STRING containing DER-encoded GeneralNames.
    const valChild = extChildren[extChildren.length - 1];
    if (valChild.tag !== TAG_OCTET_STR) continue;
    const gnames = readTLV(valChild.val, 0);
    if (gnames.tag !== TAG_SEQUENCE) continue;
    for (const gn of iterSeq(gnames.val)) {
      if (gn.tag === TAG_CONTEXT_2) { // [2] dNSName
        return new TextDecoder("ascii").decode(gn.val);
      }
    }
  }
  return "";
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

/** Return a fresh ArrayBuffer copy of a (possibly sub-array) Uint8Array. */
function toArrayBuffer(u: Uint8Array): ArrayBuffer {
  return u.buffer.slice(u.byteOffset, u.byteOffset + u.byteLength) as ArrayBuffer;
}

// ── Certificate chain verification ───────────────────────────────────────────

/**
 * Verify cert's TBSCertificate signature with the issuer's P-256 SPKI.
 * Returns the issuer's CryptoKey (for downstream use), or throws on failure.
 */
async function verifyCertSig(cert: ParsedCert, issuerSPKI: Uint8Array): Promise<CryptoKey> {
  const issuerPub = await crypto.subtle.importKey(
    "spki",
    toArrayBuffer(issuerSPKI),
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
  const sig = derToP1363(cert.sigDER);
  const ok = await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    issuerPub,
    toArrayBuffer(sig),
    toArrayBuffer(cert.tbsBytes),
  );
  if (!ok) throw new Error("x509: certificate signature does not verify");
  return issuerPub;
}

/**
 * Parse and verify the node's PEM cert chain against the pinned root PEM.
 *
 * chain is the CP-relayed cert chain (leaf + optional intermediates, PEM-concatenated).
 * rootPEM is the client-pinned Root CA PEM (embedded in the web bundle).
 *
 * Returns the verified leaf ParsedCert on success, or throws on any failure.
 */
export async function verifyCertChain(chain: string, rootPEM: string): Promise<ParsedCert> {
  const chainDERs = pemToDerList(chain);
  const rootDERs  = pemToDerList(rootPEM);
  if (rootDERs.length === 0) throw new Error("x509: empty pinned root PEM");
  if (chainDERs.length === 0) throw new Error("x509: empty cert chain PEM");

  const root    = parseCertDER(rootDERs[0]);
  const parsed  = chainDERs.map(parseCertDER);

  // Verify each cert is signed by the next in line; last cert is verified by root.
  for (let i = 0; i < parsed.length; i++) {
    const issuerSPKI = i + 1 < parsed.length ? parsed[i + 1].spkiBytes : root.spkiBytes;
    await verifyCertSig(parsed[i], issuerSPKI);
  }
  // Verify root is self-signed.
  await verifyCertSig(root, root.spkiBytes);

  return parsed[0]; // leaf
}

/**
 * Import the P-256 public key from a parsed cert's SPKI as a verify-only CryptoKey.
 */
export async function importCertPubKey(cert: ParsedCert): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "spki",
    toArrayBuffer(cert.spkiBytes),
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
}

/**
 * Parse the node identity from a verified leaf cert's SAN DNS name.
 * SAN format: <nodeId>.<accountId>.<class>.nodes.spawnery.internal
 */
export function parseSANIdentity(sanDNS: string): { nodeId: string; accountId: string; nodeClass: string } {
  const suffix = ".nodes.spawnery.internal";
  if (!sanDNS.endsWith(suffix)) throw new Error(`x509: SAN ${JSON.stringify(sanDNS)} does not end with ${suffix}`);
  const prefix = sanDNS.slice(0, -suffix.length);
  const parts = prefix.split(".");
  if (parts.length < 3) throw new Error(`x509: SAN prefix ${JSON.stringify(prefix)} needs at least 3 dot-segments`);
  const nodeClass = parts[parts.length - 1];
  const accountId = parts[parts.length - 2];
  const nodeId    = parts.slice(0, parts.length - 2).join(".");
  return { nodeId, accountId, nodeClass };
}
