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
const TAG_BOOLEAN   = 0x01;
const TAG_CONTEXT_3 = 0xa3; // [3] EXPLICIT Extensions
const TAG_CONTEXT_2 = 0x82; // [2] dNSName in GeneralName CHOICE
const TAG_UTCTIME   = 0x17; // UTCTime
const TAG_GENTIME   = 0x18; // GeneralizedTime

// ── OID byte sequences ────────────────────────────────────────────────────────

/** SAN OID 2.5.29.17 in DER: 55 1d 11. */
const SAN_OID               = new Uint8Array([0x55, 0x1d, 0x11]);
/** BasicConstraints OID 2.5.29.19 in DER: 55 1d 13. */
const BASIC_CONSTRAINTS_OID = new Uint8Array([0x55, 0x1d, 0x13]);
/** KeyUsage OID 2.5.29.15 in DER: 55 1d 0f. */
const KEY_USAGE_OID         = new Uint8Array([0x55, 0x1d, 0x0f]);
/** NameConstraints OID 2.5.29.30 in DER: 55 1d 1e. */
const NAME_CONSTRAINTS_OID  = new Uint8Array([0x55, 0x1d, 0x1e]);

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
  /** Certificate validity start (notBefore). */
  notBefore: Date;
  /** Certificate validity end (notAfter). */
  notAfter: Date;
  /** basicConstraints cA=TRUE: this cert is a CA and may sign other certs. */
  isCA: boolean;
  /** keyUsage has keyCertSign bit set. */
  certSignKeyUsage: boolean;
  /** nameConstraints permittedSubtrees dNSNames (empty = no constraint). */
  permittedDNSDomains: string[];
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
  const _outerHdrLen = tbsChild.startOff + (der.byteOffset - outer.val.byteOffset + outer.val.byteOffset - outer.val.byteOffset);
  // Simpler: the TBSCertificate full TLV starts at (outer tag+len offset + tbsChild.startOff).
  // We compute the outer header length = der.length - outer.val.length.
  const outerHeaderLen = der.length - outer.val.length;
  const tbsStart = outerHeaderLen + tbsChild.startOff;
  const tbsEnd   = outerHeaderLen + tbsChild.next;
  const tbsBytes = der.subarray(tbsStart, tbsEnd);

  // BIT STRING: first byte = unused-bit count; must be 0 for ECDSA-P256.
  if (sigChild.val[0] !== 0) throw new Error("x509: signature BIT STRING has non-zero unused bits");
  const sigDER = sigChild.val.subarray(1);

  const { spkiBytes, sanDNS, notBefore, notAfter, isCA, certSignKeyUsage, permittedDNSDomains } =
    parseTBS(tbsChild.val, outer.val);

  return { tbsBytes, sigDER, spkiBytes, sanDNS, notBefore, notAfter, isCA, certSignKeyUsage, permittedDNSDomains };
}

interface TBSFields {
  spkiBytes:           Uint8Array;
  sanDNS:              string;
  notBefore:           Date;
  notAfter:            Date;
  isCA:                boolean;
  certSignKeyUsage:    boolean;
  permittedDNSDomains: string[];
}

/**
 * Parse TBSCertificate body for SPKI, SAN, validity, and constraint extensions.
 *
 * TBSCertificate contains (RFC 5280 order):
 *   [0] version (optional), serialNumber, signature, issuer, validity, subject,
 *   SubjectPublicKeyInfo, [3] extensions (optional)
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
  const spkiBytes = tbsVal.subarray(spkiItem.startOff, spkiItem.next);

  // Validity SEQUENCE is 2 positions before SPKI (subject occupies the slot between them).
  const validityIdx = spkiIdx - 2;
  if (validityIdx < 0 || items[validityIdx].tag !== TAG_SEQUENCE) {
    throw new Error("x509: could not locate Validity field in TBSCertificate");
  }
  const { notBefore, notAfter } = _parseValidity(items[validityIdx].val);

  // Parse all relevant extensions.
  let sanDNS = "";
  let isCA = false;
  let certSignKeyUsage = false;
  let permittedDNSDomains: string[] = [];
  if (extsIdx >= 0) {
    const exts = _parseExtensions(items[extsIdx].val);
    sanDNS             = exts.sanDNS;
    isCA               = exts.isCA;
    certSignKeyUsage   = exts.certSignKeyUsage;
    permittedDNSDomains = exts.permittedDNSDomains;
  }

  void certVal;
  return { spkiBytes, sanDNS, notBefore, notAfter, isCA, certSignKeyUsage, permittedDNSDomains };
}

// ── Time parsing ──────────────────────────────────────────────────────────────

function _parseValidity(validityVal: Uint8Array): { notBefore: Date; notAfter: Date } {
  const times = [...iterSeq(validityVal)];
  if (times.length < 2) throw new Error("x509: Validity has fewer than 2 time values");
  return { notBefore: _parseDERTime(times[0]), notAfter: _parseDERTime(times[1]) };
}

function _parseDERTime(tlv: TLV): Date {
  if (tlv.tag !== TAG_UTCTIME && tlv.tag !== TAG_GENTIME) {
    throw new Error(`x509: unexpected time tag 0x${tlv.tag.toString(16)} in Validity`);
  }
  const s = new TextDecoder("ascii").decode(tlv.val);
  if (tlv.tag === TAG_UTCTIME) {
    // YYMMDDHHMMSSZ — RFC 5280 mandates Z and seconds
    const yy = parseInt(s.slice(0, 2), 10);
    const year = yy >= 50 ? 1900 + yy : 2000 + yy;
    return new Date(Date.UTC(
      year,
      parseInt(s.slice(2, 4), 10) - 1,
      parseInt(s.slice(4, 6), 10),
      parseInt(s.slice(6, 8), 10),
      parseInt(s.slice(8, 10), 10),
      parseInt(s.slice(10, 12), 10),
    ));
  } else {
    // YYYYMMDDHHMMSSZ (GeneralizedTime)
    return new Date(Date.UTC(
      parseInt(s.slice(0, 4), 10),
      parseInt(s.slice(4, 6), 10) - 1,
      parseInt(s.slice(6, 8), 10),
      parseInt(s.slice(8, 10), 10),
      parseInt(s.slice(10, 12), 10),
      parseInt(s.slice(12, 14), 10),
    ));
  }
}

// ── Extension parsing ─────────────────────────────────────────────────────────

interface _ParsedExtensions {
  sanDNS:             string;
  isCA:               boolean;
  certSignKeyUsage:   boolean;
  permittedDNSDomains: string[];
}

/**
 * Parse all security-relevant extensions from the [3] Extensions body.
 * extsBody is the value of the [3] EXPLICIT wrapper (contains SEQUENCE OF Extension).
 */
function _parseExtensions(extsBody: Uint8Array): _ParsedExtensions {
  const extsList = readTLV(extsBody, 0);
  if (extsList.tag !== TAG_SEQUENCE) throw new Error("x509: extensions outer is not a SEQUENCE");

  let sanDNS = "";
  let isCA = false;
  let certSignKeyUsage = false;
  let permittedDNSDomains: string[] = [];

  for (const ext of iterSeq(extsList.val)) {
    if (ext.tag !== TAG_SEQUENCE) continue;
    const extChildren = [...iterSeq(ext.val)];
    if (extChildren.length < 2) continue;
    if (extChildren[0].tag !== TAG_OID) continue;
    const oidBytes = extChildren[0].val;
    // Last element is OCTET STRING containing the extension value.
    const valChild = extChildren[extChildren.length - 1];
    if (valChild.tag !== TAG_OCTET_STR) continue;
    const octVal = valChild.val;

    if (bytesEqual(oidBytes, SAN_OID)) {
      const gnames = readTLV(octVal, 0);
      if (gnames.tag !== TAG_SEQUENCE) continue;
      for (const gn of iterSeq(gnames.val)) {
        if (gn.tag === TAG_CONTEXT_2 && sanDNS === "") {
          sanDNS = new TextDecoder("ascii").decode(gn.val);
        }
      }
    } else if (bytesEqual(oidBytes, BASIC_CONSTRAINTS_OID)) {
      isCA = _parseIsCA(octVal);
    } else if (bytesEqual(oidBytes, KEY_USAGE_OID)) {
      certSignKeyUsage = _parseCertSignKeyUsage(octVal);
    } else if (bytesEqual(oidBytes, NAME_CONSTRAINTS_OID)) {
      permittedDNSDomains = _parsePermittedDNSDomains(octVal);
    }
  }

  return { sanDNS, isCA, certSignKeyUsage, permittedDNSDomains };
}

/** Parse BasicConstraints OCTET STRING value; returns true iff cA=TRUE. */
function _parseIsCA(octVal: Uint8Array): boolean {
  // BasicConstraints ::= SEQUENCE { cA BOOLEAN OPTIONAL, pathLenConstraint INTEGER OPTIONAL }
  const seq = readTLV(octVal, 0);
  if (seq.tag !== TAG_SEQUENCE) return false;
  for (const item of iterSeq(seq.val)) {
    if (item.tag === TAG_BOOLEAN && item.val.length > 0) return item.val[0] !== 0;
  }
  return false; // cA absent → default FALSE
}

/**
 * Parse KeyUsage BIT STRING value; returns true iff keyCertSign (bit 5) is set.
 * RFC 5280: keyCertSign = bit 5 from the MSB → 0x04 in the first content byte.
 */
function _parseCertSignKeyUsage(octVal: Uint8Array): boolean {
  const bs = readTLV(octVal, 0);
  if (bs.tag !== TAG_BIT_STR || bs.val.length < 2) return false;
  // bs.val[0] = unused-bit count; bs.val[1] = first content byte.
  return (bs.val[1] & 0x04) !== 0;
}

/**
 * Parse NameConstraints OCTET STRING value; returns the permitted DNS subtrees.
 * RFC 5280: permittedSubtrees [0] IMPLICIT GeneralSubtrees; dNSName [2] IA5String.
 */
function _parsePermittedDNSDomains(octVal: Uint8Array): string[] {
  // NameConstraints ::= SEQUENCE { [0] permittedSubtrees, [1] excludedSubtrees }
  const nc = readTLV(octVal, 0);
  if (nc.tag !== TAG_SEQUENCE) return [];
  const permitted: string[] = [];
  for (const item of iterSeq(nc.val)) {
    if (item.tag !== 0xa0) continue; // [0] IMPLICIT on SEQUENCE OF GeneralSubtree
    // item.val is the content of the GeneralSubtrees (no outer SEQUENCE TLV, IMPLICIT tag)
    for (const subtree of iterSeq(item.val)) {
      if (subtree.tag !== TAG_SEQUENCE) continue;
      if (subtree.val.length === 0) continue;
      const base = readTLV(subtree.val, 0);
      if (base.tag === TAG_CONTEXT_2) { // [2] dNSName
        permitted.push(new TextDecoder("ascii").decode(base.val));
      }
    }
  }
  return permitted;
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
 * now is the reference time for validity checks (injectable for testing).
 *
 * Enforced on top of signature verification (mirrors crypto/x509 behaviour):
 *   (a) nameConstraints — issuer's permittedDNSDomains must cover the signed cert's SAN
 *   (b) basicConstraints CA:TRUE + keyUsage keyCertSign on every issuing cert
 *   (c) notBefore/notAfter validity window on every cert in the chain
 *
 * Returns the verified leaf ParsedCert on success, or throws on any failure.
 */
export async function verifyCertChain(chain: string, rootPEM: string, now: Date): Promise<ParsedCert> {
  const chainDERs = pemToDerList(chain);
  const rootDERs  = pemToDerList(rootPEM);
  if (rootDERs.length === 0) throw new Error("x509: empty pinned root PEM");
  if (chainDERs.length === 0) throw new Error("x509: empty cert chain PEM");

  const root   = parseCertDER(rootDERs[0]);
  const parsed = chainDERs.map(parseCertDER);

  // Verify root is self-signed, is a CA, has certSign usage, and is within validity.
  await verifyCertSig(root, root.spkiBytes);
  if (!root.isCA) throw new Error("x509: pinned root cert lacks basicConstraints CA:TRUE");
  if (!root.certSignKeyUsage) throw new Error("x509: pinned root cert lacks keyCertSign key usage");
  _checkValidity(root, now, "root");

  // Verify each cert in the chain.
  for (let i = 0; i < parsed.length; i++) {
    const issuerCert = i + 1 < parsed.length ? parsed[i + 1] : root;

    // (b) Intermediate issuers (not the pre-checked root) must have CA:TRUE + certSign.
    if (i + 1 < parsed.length) {
      if (!issuerCert.isCA) {
        throw new Error(`x509: chain[${i + 1}] lacks basicConstraints CA:TRUE`);
      }
      if (!issuerCert.certSignKeyUsage) {
        throw new Error(`x509: chain[${i + 1}] lacks keyCertSign key usage`);
      }
    }

    // Signature check.
    await verifyCertSig(parsed[i], issuerCert.spkiBytes);

    // (c) Validity window.
    _checkValidity(parsed[i], now, `chain[${i}]`);

    // (a) Name constraints: issuer's permittedDNSDomains must cover the signed cert's SAN.
    if (issuerCert.permittedDNSDomains.length > 0 && parsed[i].sanDNS) {
      if (!_dnsInPermitted(parsed[i].sanDNS, issuerCert.permittedDNSDomains)) {
        throw new Error(
          `x509: cert SAN ${JSON.stringify(parsed[i].sanDNS)} violates issuer name constraints ` +
          `[${issuerCert.permittedDNSDomains.join(", ")}]`,
        );
      }
    }
  }

  return parsed[0]; // leaf
}

function _checkValidity(cert: ParsedCert, now: Date, label: string): void {
  if (now < cert.notBefore) {
    throw new Error(`x509: ${label} cert not yet valid (notBefore=${cert.notBefore.toISOString()})`);
  }
  if (now >= cert.notAfter) {
    throw new Error(`x509: ${label} cert has expired (notAfter=${cert.notAfter.toISOString()})`);
  }
}

/** Returns true if san equals or is a subdomain of any entry in the permitted list. */
function _dnsInPermitted(san: string, permitted: string[]): boolean {
  return permitted.some((domain) => san === domain || san.endsWith("." + domain));
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
