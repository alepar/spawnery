/**
 * Narrowed X.509 DER parser for Spawnery's P-256 node cert profile.
 *
 * Parses only the fields we need:
 *   - TBSCertificate bytes (signed portion, for chain verification)
 *   - Signature bytes (ECDSA-ASN.1, converted to P1363 for SubtleCrypto)
 *   - SubjectPublicKeyInfo (raw SPKI bytes for SubtleCrypto.importKey)
 *   - SubjectAltName GeneralNames and certificatePolicies
 *
 * Our node cert profile:
 *   - ECDSA-SHA256 signature algorithm
 *   - P-256 (secp256r1) EC key
 *   - exactly one SPIFFE URI SAN
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
const TAG_CONTEXT_6 = 0x86; // [6] uniformResourceIdentifier
const TAG_CONTEXT_7 = 0x87; // [7] iPAddress
const TAG_UTCTIME   = 0x17; // UTCTime
const TAG_GENTIME   = 0x18; // GeneralizedTime

// ── OID byte sequences ────────────────────────────────────────────────────────

/** SAN OID 2.5.29.17 in DER: 55 1d 11. */
const SAN_OID               = new Uint8Array([0x55, 0x1d, 0x11]);
/** BasicConstraints OID 2.5.29.19 in DER: 55 1d 13. */
const BASIC_CONSTRAINTS_OID = new Uint8Array([0x55, 0x1d, 0x13]);
/** KeyUsage OID 2.5.29.15 in DER: 55 1d 0f. */
const KEY_USAGE_OID         = new Uint8Array([0x55, 0x1d, 0x0f]);
/** ExtendedKeyUsage OID 2.5.29.37. */
const EXT_KEY_USAGE_OID     = new Uint8Array([0x55, 0x1d, 0x25]);
/** CertificatePolicies OID 2.5.29.32. */
const CERT_POLICIES_OID     = new Uint8Array([0x55, 0x1d, 0x20]);
const SUBJECT_KEY_ID_OID    = new Uint8Array([0x55, 0x1d, 0x0e]);
const AUTHORITY_KEY_ID_OID  = new Uint8Array([0x55, 0x1d, 0x23]);

const CLIENT_AUTH_OID = "1.3.6.1.5.5.7.3.2";
const SERVER_AUTH_OID = "1.3.6.1.5.5.7.3.1";
const SERVICE_ISSUER_POLICY = "2.25.252512432928806341888652597142698706330";
const CLOUD_NODE_ISSUER_POLICY = "2.25.272377079450377973232136459441396509550";
const SELF_HOSTED_NODE_ISSUER_POLICY = "2.25.13905568351287903487917266049020976148";

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
  sanURIs: string[];
  sanDNS: string[];
  sanIPs: string[];
  /** Certificate validity start (notBefore). */
  notBefore: Date;
  /** Certificate validity end (notAfter). */
  notAfter: Date;
  /** basicConstraints cA=TRUE: this cert is a CA and may sign other certs. */
  isCA: boolean;
  hasBasicConstraints: boolean;
  digitalSignatureOnly: boolean;
  /** keyUsage has keyCertSign bit set. */
  certSignKeyUsage: boolean;
  extendedKeyUsages: string[];
  policies: string[];
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

  const { spkiBytes, sanURIs, sanDNS, sanIPs, notBefore, notAfter, isCA, hasBasicConstraints, digitalSignatureOnly, certSignKeyUsage, extendedKeyUsages, policies } =
    parseTBS(tbsChild.val, outer.val);

  return { tbsBytes, sigDER, spkiBytes, sanURIs, sanDNS, sanIPs, notBefore, notAfter, isCA, hasBasicConstraints, digitalSignatureOnly, certSignKeyUsage, extendedKeyUsages, policies };
}

interface TBSFields {
  spkiBytes:           Uint8Array;
  sanURIs:             string[];
  sanDNS:              string[];
  sanIPs:              string[];
  notBefore:           Date;
  notAfter:            Date;
  isCA:                boolean;
  hasBasicConstraints: boolean;
  digitalSignatureOnly: boolean;
  certSignKeyUsage:    boolean;
  extendedKeyUsages:   string[];
  policies:            string[];
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
  let sanURIs: string[] = [];
  let sanDNS: string[] = [];
  let sanIPs: string[] = [];
  let isCA = false;
  let hasBasicConstraints = false;
  let digitalSignatureOnly = false;
  let certSignKeyUsage = false;
  let extendedKeyUsages: string[] = [];
  let policies: string[] = [];
  if (extsIdx >= 0) {
    const exts = _parseExtensions(items[extsIdx].val);
    ({ sanURIs, sanDNS, sanIPs, isCA, hasBasicConstraints, digitalSignatureOnly, certSignKeyUsage, extendedKeyUsages, policies } = exts);
  }

  void certVal;
  return { spkiBytes, sanURIs, sanDNS, sanIPs, notBefore, notAfter, isCA, hasBasicConstraints, digitalSignatureOnly, certSignKeyUsage, extendedKeyUsages, policies };
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
  const expectedLength = tlv.tag === TAG_UTCTIME ? 13 : 15;
  if (s.length !== expectedLength || !/^\d+Z$/.test(s)) throw new Error("x509: malformed RFC5280 time");
  let year: number;
  let offset: number;
  if (tlv.tag === TAG_UTCTIME) {
    const yy = parseInt(s.slice(0, 2), 10);
    year = yy >= 50 ? 1900 + yy : 2000 + yy;
    offset = 2;
  } else {
    year = parseInt(s.slice(0, 4), 10);
    offset = 4;
  }
  const month = parseInt(s.slice(offset, offset + 2), 10);
  const day = parseInt(s.slice(offset + 2, offset + 4), 10);
  const hour = parseInt(s.slice(offset + 4, offset + 6), 10);
  const minute = parseInt(s.slice(offset + 6, offset + 8), 10);
  const second = parseInt(s.slice(offset + 8, offset + 10), 10);
  if (month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59) throw new Error("x509: invalid RFC5280 calendar time");
  const result = new Date(Date.UTC(year, month - 1, day, hour, minute, second));
  if (!Number.isFinite(result.getTime()) || result.getUTCFullYear() !== year || result.getUTCMonth() !== month - 1 || result.getUTCDate() !== day || result.getUTCHours() !== hour || result.getUTCMinutes() !== minute || result.getUTCSeconds() !== second) {
    throw new Error("x509: invalid RFC5280 calendar time");
  }
  return result;
}

// ── Extension parsing ─────────────────────────────────────────────────────────

interface _ParsedExtensions {
  sanURIs:            string[];
  sanDNS:             string[];
  sanIPs:             string[];
  isCA:               boolean;
  hasBasicConstraints: boolean;
  digitalSignatureOnly: boolean;
  certSignKeyUsage:   boolean;
  extendedKeyUsages:  string[];
  policies:           string[];
}

/**
 * Parse all security-relevant extensions from the [3] Extensions body.
 * extsBody is the value of the [3] EXPLICIT wrapper (contains SEQUENCE OF Extension).
 */
function _parseExtensions(extsBody: Uint8Array): _ParsedExtensions {
  const extsList = readTLV(extsBody, 0);
  if (extsList.tag !== TAG_SEQUENCE || extsList.next !== extsBody.length) throw new Error("x509: extensions outer is not a complete SEQUENCE");

  const sanURIs: string[] = [];
  const sanDNS: string[] = [];
  const sanIPs: string[] = [];
  let isCA = false;
  let hasBasicConstraints = false;
  let digitalSignatureOnly = false;
  let certSignKeyUsage = false;
  let extendedKeyUsages: string[] = [];
  let policies: string[] = [];
  let sanCount = 0;
  const seenExtensions = new Set<string>();

  for (const ext of iterSeq(extsList.val)) {
    if (ext.tag !== TAG_SEQUENCE) throw new Error("x509: malformed Extension entry");
    const extChildren = [...iterSeq(ext.val)];
    if (extChildren.length !== 2 && extChildren.length !== 3) throw new Error("x509: malformed Extension fields");
    if (extChildren[0].tag !== TAG_OID) throw new Error("x509: Extension lacks OID");
    const oidBytes = extChildren[0].val;
    const extensionKey = Array.from(oidBytes).join(".");
    if (seenExtensions.has(extensionKey)) throw new Error("x509: duplicate certificate extension");
    seenExtensions.add(extensionKey);
    let critical = false;
    if (extChildren.length === 3) {
      const criticalField = extChildren[1];
      // Extension.critical has DEFAULT FALSE, so DER permits the field only for TRUE.
      if (criticalField.tag !== TAG_BOOLEAN || criticalField.val.length !== 1 || criticalField.val[0] !== 0xff) {
        throw new Error("x509: malformed Extension critical BOOLEAN");
      }
      critical = criticalField.val[0] === 0xff;
    }
    const valChild = extChildren[extChildren.length - 1];
    if (valChild.tag !== TAG_OCTET_STR) throw new Error("x509: Extension value is not an OCTET STRING");
    const octVal = valChild.val;
    const recognized = bytesEqual(oidBytes, SAN_OID) || bytesEqual(oidBytes, BASIC_CONSTRAINTS_OID) || bytesEqual(oidBytes, KEY_USAGE_OID) || bytesEqual(oidBytes, EXT_KEY_USAGE_OID) || bytesEqual(oidBytes, CERT_POLICIES_OID) || bytesEqual(oidBytes, SUBJECT_KEY_ID_OID) || bytesEqual(oidBytes, AUTHORITY_KEY_ID_OID);
    if (critical && !recognized) throw new Error(`x509: unrecognized critical extension ${_decodeOID(oidBytes)}`);

    if (bytesEqual(oidBytes, SAN_OID)) {
      sanCount++;
      if (sanCount !== 1) throw new Error("x509: duplicate subjectAltName extension");
      const gnames = readTLV(octVal, 0);
      if (gnames.tag !== TAG_SEQUENCE || gnames.next !== octVal.length) throw new Error("x509: malformed subjectAltName extension");
      for (const gn of iterSeq(gnames.val)) {
        if (gn.tag === TAG_CONTEXT_6) sanURIs.push(new TextDecoder("ascii").decode(gn.val));
        else if (gn.tag === TAG_CONTEXT_2) {
          const dns = new TextDecoder("ascii").decode(gn.val);
          if (sanDNS.some((value) => value.toLowerCase() === dns.toLowerCase())) throw new Error("x509: duplicate DNS GeneralName");
          sanDNS.push(dns);
        } else if (gn.tag === TAG_CONTEXT_7) {
          const ip = Array.from(gn.val).join(".");
          if (sanIPs.includes(ip)) throw new Error("x509: duplicate IP GeneralName");
          sanIPs.push(ip);
        }
        else throw new Error(`x509: unsupported GeneralName tag 0x${gn.tag.toString(16)}`);
      }
    } else if (bytesEqual(oidBytes, BASIC_CONSTRAINTS_OID)) {
      hasBasicConstraints = true;
      isCA = _parseIsCA(octVal);
    } else if (bytesEqual(oidBytes, KEY_USAGE_OID)) {
      certSignKeyUsage = _parseCertSignKeyUsage(octVal);
      digitalSignatureOnly = _parseDigitalSignatureOnly(octVal);
    } else if (bytesEqual(oidBytes, EXT_KEY_USAGE_OID)) {
      extendedKeyUsages = _parseOIDSequence(octVal);
    } else if (bytesEqual(oidBytes, CERT_POLICIES_OID)) {
      policies = _parseCertificatePolicies(octVal);
    }
  }

  return { sanURIs, sanDNS, sanIPs, isCA, hasBasicConstraints, digitalSignatureOnly, certSignKeyUsage, extendedKeyUsages, policies };
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

function _parseDigitalSignatureOnly(octVal: Uint8Array): boolean {
  const bs = readTLV(octVal, 0);
  if (bs.tag !== TAG_BIT_STR || bs.val.length < 2 || bs.val[0] > 7 || bs.val[1] !== 0x80) return false;
  for (let i = 2; i < bs.val.length; i++) if (bs.val[i] !== 0) return false;
  return true;
}

function _parseOIDSequence(octVal: Uint8Array): string[] {
  const seq = readTLV(octVal, 0);
  if (seq.tag !== TAG_SEQUENCE || seq.next !== octVal.length) throw new Error("x509: malformed OID sequence");
  return [...iterSeq(seq.val)].map((item) => {
    if (item.tag !== TAG_OID) throw new Error("x509: expected OID");
    return _decodeOID(item.val);
  });
}

function _parseCertificatePolicies(octVal: Uint8Array): string[] {
  const seq = readTLV(octVal, 0);
  if (seq.tag !== TAG_SEQUENCE || seq.next !== octVal.length) throw new Error("x509: malformed certificatePolicies");
  return [...iterSeq(seq.val)].map((info) => {
    if (info.tag !== TAG_SEQUENCE) throw new Error("x509: malformed PolicyInformation");
    const fields = [...iterSeq(info.val)];
    if (fields.length === 0 || fields[0].tag !== TAG_OID) throw new Error("x509: policy lacks OID");
    return _decodeOID(fields[0].val);
  });
}

function _decodeOID(value: Uint8Array): string {
  const arcs: bigint[] = [];
  let current = 0n;
  for (const b of value) {
    current = (current << 7n) | BigInt(b & 0x7f);
    if ((b & 0x80) === 0) {
      arcs.push(current);
      current = 0n;
    }
  }
  if (current !== 0n || arcs.length === 0) throw new Error("x509: malformed OID");
  const first = arcs.shift()!;
  const firstArc = first < 40n ? 0n : first < 80n ? 1n : 2n;
  const secondArc = first - firstArc * 40n;
  return [firstArc, secondArc, ...arcs].map(String).join(".");
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

// ── Certificate chain verification ───────────────────────────────────────────

/**
 * Verify cert's TBSCertificate signature with the issuer's P-256 SPKI.
 * Returns the issuer's CryptoKey (for downstream use), or throws on failure.
 */
async function verifyCertSig(cert: ParsedCert, issuerSPKI: Uint8Array): Promise<CryptoKey> {
  // TS 5.9: Uint8Array<ArrayBufferLike> not assignable to BufferSource; cast is safe —
  // all these Uint8Arrays are backed by plain ArrayBuffers (no SharedArrayBuffer).
  // Pass Uint8Array directly rather than converting to ArrayBuffer: jsdom's SubtleCrypto
  // performs a realm-specific instanceof check on ArrayBuffer which fails for buffers
  // created outside the jsdom realm (e.g., via .buffer.slice()).
  const issuerPub = await crypto.subtle.importKey(
    "spki",
    issuerSPKI as unknown as Uint8Array<ArrayBuffer>,
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
  const sig = derToP1363(cert.sigDER);
  const ok = await crypto.subtle.verify(
    { name: "ECDSA", hash: "SHA-256" },
    issuerPub,
    sig as unknown as Uint8Array<ArrayBuffer>,
    cert.tbsBytes as unknown as Uint8Array<ArrayBuffer>,
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
 * Enforces the Spawnery X.509-SVID profile and issuer policy after signature validation.
 *
 * Returns the verified leaf ParsedCert on success, or throws on any failure.
 */
export async function verifyCertChain(chain: string, rootPEM: string, now: Date, trustDomain: string): Promise<ParsedCert> {
  if (!trustDomain) throw new Error("x509: configured trust domain is required");
  const chainDERs = pemToDerList(chain);
  const rootDERs  = pemToDerList(rootPEM);
  if (rootDERs.length === 0) throw new Error("x509: empty pinned root PEM");
  if (chainDERs.length === 0) throw new Error("x509: empty cert chain PEM");

  const root   = parseCertDER(rootDERs[0]);
  const parsed = chainDERs.map(parseCertDER);
  if (parsed.length !== 2) throw new Error("x509: chain must contain leaf and one signing intermediate");

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

  }

  const leaf = parsed[0];
  const issuer = parsed[1];
  if (issuer.sanURIs.length !== 1 || issuer.sanURIs[0] !== `spiffe://${trustDomain}`) {
    throw new Error("x509: signing intermediate URI SAN does not match configured trust domain");
  }
  if (issuer.policies.length !== 1) throw new Error("x509: signing intermediate must contain exactly one issuer policy");
  if (!leaf.hasBasicConstraints || leaf.isCA || !leaf.digitalSignatureOnly) {
    throw new Error("x509: leaf violates basicConstraints or DigitalSignature-only profile");
  }
  if (leaf.extendedKeyUsages.length !== 2 || !leaf.extendedKeyUsages.includes(CLIENT_AUTH_OID) || !leaf.extendedKeyUsages.includes(SERVER_AUTH_OID)) {
    throw new Error("x509: leaf must contain exactly ClientAuth and ServerAuth EKUs");
  }
  if (leaf.sanURIs.length !== 1) throw new Error("x509: leaf must contain exactly one URI SAN");
  const principal = parseSPIFFEPrincipal(leaf.sanURIs[0], trustDomain);
  if (principal.kind === "node" && (leaf.sanDNS.length !== 0 || leaf.sanIPs.length !== 0)) {
    throw new Error("x509: node leaf must not contain DNS or IP SANs");
  }
  const policy = issuer.policies[0];
  const permitted = policy === SERVICE_ISSUER_POLICY
    ? principal.kind === "service"
    : policy === CLOUD_NODE_ISSUER_POLICY
      ? principal.kind === "node" && principal.role === "cloud"
      : policy === SELF_HOSTED_NODE_ISSUER_POLICY
        ? principal.kind === "node" && principal.role === "self-hosted"
        : false;
  if (!permitted) throw new Error("x509: issuer policy does not permit SPIFFE principal path");

  return leaf;
}

function _checkValidity(cert: ParsedCert, now: Date, label: string): void {
  if (now < cert.notBefore) {
    throw new Error(`x509: ${label} cert not yet valid (notBefore=${cert.notBefore.toISOString()})`);
  }
  if (now >= cert.notAfter) {
    throw new Error(`x509: ${label} cert has expired (notAfter=${cert.notAfter.toISOString()})`);
  }
}

/**
 * Import the P-256 public key from a parsed cert's SPKI as a verify-only CryptoKey.
 */
export async function importCertPubKey(cert: ParsedCert): Promise<CryptoKey> {
  // TS 5.9 cast — see verifyCertSig comment above.
  return crypto.subtle.importKey(
    "spki",
    cert.spkiBytes as unknown as Uint8Array<ArrayBuffer>,
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
}

export type SPIFFEPrincipal =
  | { trustDomain: string; kind: "service"; role: "cp" | "authsvc"; instanceId: string }
  | { trustDomain: string; kind: "node"; role: "cloud" | "self-hosted"; accountId: string; nodeId: string };

export function parseSPIFFEPrincipal(raw: string, trustDomain: string): SPIFFEPrincipal {
  if (!/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(trustDomain) || trustDomain.includes("..")) {
    throw new Error("x509: configured trust domain is not canonical");
  }
  const prefix = `spiffe://${trustDomain}/`;
  if (!raw.startsWith(prefix) || raw.includes("%") || raw.includes("?") || raw.includes("#") || raw.includes("@")) throw new Error("x509: non-canonical SPIFFE URI SAN");
  const path = raw.slice(prefix.length);
  const segments = path.split("/");
  if (segments.some((s) => !/^[A-Za-z0-9._-]+$/.test(s) || s === "." || s === "..")) {
    throw new Error("x509: invalid SPIFFE path segment");
  }
  if (raw !== `${prefix}${segments.join("/")}`) throw new Error("x509: non-canonical SPIFFE URI SAN");
  if (segments.length === 3 && segments[0] === "service" && (segments[1] === "cp" || segments[1] === "authsvc")) {
    return { trustDomain, kind: "service", role: segments[1], instanceId: segments[2] };
  }
  if (segments.length === 4 && segments[0] === "node" && (segments[1] === "cloud" || segments[1] === "self-hosted")) {
    return { trustDomain, kind: "node", role: segments[1], accountId: segments[2], nodeId: segments[3] };
  }
  throw new Error("x509: unsupported SPIFFE principal path");
}
