import { BitString, Integer, OctetString, Sequence, fromBER } from "asn1js";
import {
  AltName,
  AuthorityKeyIdentifier,
  BasicConstraints,
  Certificate,
  CertificateRevocationList,
  CryptoEngine,
} from "pkijs";

import type { Extension } from "pkijs";
import type { NodeCRLTrustInput } from "../../build/trust-inputs";
import {
  parseCertificatePoliciesDER,
  pemToDerList,
  validateTrustDomain,
} from "@/keys/x509";

const BASIC_CONSTRAINTS_OID = "2.5.29.19";
const KEY_USAGE_OID = "2.5.29.15";
const SUBJECT_KEY_ID_OID = "2.5.29.14";
const SUBJECT_ALT_NAME_OID = "2.5.29.17";
const CERTIFICATE_POLICIES_OID = "2.5.29.32";
const AUTHORITY_KEY_ID_OID = "2.5.29.35";
const CRL_NUMBER_OID = "2.5.29.20";
const MAX_CRL_DER_SIZE = 4 << 20;

const ISSUER_ROLE_POLICIES = new Set([
  "2.25.252512432928806341888652597142698706330",
  "2.25.272377079450377973232136459441396509550",
  "2.25.13905568351287903487917266049020976148",
]);

class RealmSafeCryptoEngine extends CryptoEngine {
  override verify(
    algorithm: AlgorithmIdentifier | RsaPssParams | EcdsaParams,
    key: CryptoKey,
    signature: BufferSource,
    data: BufferSource,
  ): Promise<boolean> {
    return this.subtle.verify(algorithm, key, byteView(signature), byteView(data));
  }
}

function byteView(
  source: ArrayBufferLike | ArrayBufferView<ArrayBufferLike>,
): Uint8Array<ArrayBuffer> {
  const view = ArrayBuffer.isView(source)
    ? new Uint8Array(source.buffer, source.byteOffset, source.byteLength)
    : new Uint8Array(source);
  const copy = new Uint8Array(view.length);
  copy.set(view);
  return copy;
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function bytesCompare(left: Uint8Array, right: Uint8Array): number {
  const shared = Math.min(left.length, right.length);
  for (let index = 0; index < shared; index++) {
    if (left[index] !== right[index]) return left[index] - right[index];
  }
  return left.length - right.length;
}

function assertCanonicalPrimitive(
  der: Uint8Array,
  tag: number,
  contentStart: number,
  contentEnd: number,
): void {
  const length = contentEnd - contentStart;
  if (tag === 0) throw new Error("end-of-contents is forbidden in DER");
  if (tag === 1 && (length !== 1 || der[contentStart] !== 0 && der[contentStart] !== 0xff)) {
    throw new Error("non-canonical BOOLEAN");
  }
  if (tag === 2 || tag === 10) {
    if (length === 0
        || length > 1 && der[contentStart] === 0 && (der[contentStart + 1] & 0x80) === 0
        || length > 1 && der[contentStart] === 0xff && (der[contentStart + 1] & 0x80) !== 0) {
      throw new Error("non-canonical INTEGER");
    }
  }
  if (tag === 3) {
    if (length === 0 || der[contentStart] > 7
        || length === 1 && der[contentStart] !== 0
        || length > 1 && der[contentEnd - 1] & (1 << der[contentStart]) - 1) {
      throw new Error("non-canonical BIT STRING");
    }
  }
  if (tag === 5 && length !== 0) throw new Error("non-canonical NULL");
  if (tag === 6) {
    if (length === 0) throw new Error("empty OBJECT IDENTIFIER");
    let start = true;
    for (let offset = contentStart; offset < contentEnd; offset++) {
      if (start && der[offset] === 0x80) throw new Error("non-canonical OBJECT IDENTIFIER");
      start = (der[offset] & 0x80) === 0;
    }
    if (!start) throw new Error("truncated OBJECT IDENTIFIER");
  }
  if (tag === 23 || tag === 24) {
    const value = String.fromCharCode(...der.subarray(contentStart, contentEnd));
    const canonical = tag === 23 ? /^\d{12}Z$/u : /^\d{14}Z$/u;
    if (!canonical.test(value)) throw new Error("non-canonical time");
  }
}

function assertCanonicalDER(der: Uint8Array, label: string): void {
  const fail = (): never => { throw new Error(`node CRL: ${label} must use canonical DER`); };

  const readElement = (start: number, limit: number): number => {
    try {
      let offset = start;
      if (offset >= limit) return fail();
      const identifier = der[offset++];
      const tagClass = identifier & 0xc0;
      const constructed = (identifier & 0x20) !== 0;
      let tag = identifier & 0x1f;
      if (tag === 0x1f) {
        tag = 0;
        let first = true;
        for (;;) {
          if (offset >= limit) return fail();
          const octet = der[offset++];
          if (first && (octet & 0x7f) === 0) return fail();
          first = false;
          tag = tag * 128 + (octet & 0x7f);
          if (!Number.isSafeInteger(tag)) return fail();
          if ((octet & 0x80) === 0) break;
        }
        if (tag < 31) return fail();
      }

      if (offset >= limit) return fail();
      const firstLength = der[offset++];
      let length = firstLength;
      if (firstLength & 0x80) {
        const octets = firstLength & 0x7f;
        if (octets === 0 || octets > 4 || offset + octets > limit || der[offset] === 0) return fail();
        length = 0;
        for (let index = 0; index < octets; index++) length = length * 256 + der[offset++];
        if (length < 128 || !Number.isSafeInteger(length)) return fail();
      }
      const contentStart = offset;
      const contentEnd = contentStart + length;
      if (contentEnd > limit) return fail();

      if (tagClass === 0 && ((tag === 16 || tag === 17) !== constructed)) return fail();
      if (tagClass === 0 && constructed && tag !== 16 && tag !== 17) return fail();
      if (constructed) {
        const children: Uint8Array[] = [];
        while (offset < contentEnd) {
          const childStart = offset;
          offset = readElement(offset, contentEnd);
          children.push(der.subarray(childStart, offset));
        }
        if (offset !== contentEnd) return fail();
        if (tagClass === 0 && tag === 17) {
          for (let index = 1; index < children.length; index++) {
            if (bytesCompare(children[index - 1], children[index]) > 0) return fail();
          }
        }
      } else if (tagClass === 0) {
        assertCanonicalPrimitive(der, tag, contentStart, contentEnd);
      }
      return contentEnd;
    } catch {
      return fail();
    }
  };

  if (readElement(0, der.length) !== der.length) fail();
}

type ASN1Block = ReturnType<typeof fromBER>["result"];

function childBlocks(block: ASN1Block): ASN1Block[] {
  const value = (block.valueBlock as unknown as { value?: ASN1Block[] }).value;
  return value ?? [];
}

function rawBlock(block: ASN1Block): Uint8Array {
  return byteView(block.valueBeforeDecodeView);
}

function parseDER<T>(
  der: Uint8Array,
  label: string,
  construct: (schema: ReturnType<typeof fromBER>["result"]) => T,
): T {
  const buffer = der.buffer.slice(der.byteOffset, der.byteOffset + der.byteLength) as ArrayBuffer;
  const parsed = fromBER(buffer);
  if (parsed.offset === -1 || parsed.offset !== der.byteLength) {
    throw new Error(`node CRL: malformed ${label} DER`);
  }
  try {
    return construct(parsed.result);
  } catch {
    throw new Error(`node CRL: malformed ${label} DER`);
  }
}

function onePEM(pem: string, label: string): Uint8Array {
  const blocks = pemToDerList(pem);
  if (blocks.length !== 1) throw new Error(`node CRL: ${label} must contain exactly one PEM block`);
  return blocks[0];
}

function certificate(der: Uint8Array, label: string): Certificate {
  return parseDER(der, label, (schema) => new Certificate({ schema }));
}

function revocationList(pem: string): CertificateRevocationList {
  const der = onePEM(pem, "CRL");
  if (der.length > MAX_CRL_DER_SIZE) throw new Error("node CRL: CRL exceeds size limit");
  assertCanonicalDER(der, "CRL");
  const buffer = der.buffer.slice(der.byteOffset, der.byteOffset + der.byteLength) as ArrayBuffer;
  const parsed = fromBER(buffer);
  if (parsed.offset === -1 || parsed.offset !== der.byteLength || !(parsed.result instanceof Sequence)) {
    throw new Error("node CRL: malformed CRL DER");
  }
  const outer = childBlocks(parsed.result);
  const tbs = outer[0];
  const tbsFields = tbs ? childBlocks(tbs) : [];
  if (outer.length !== 3 || !(tbs instanceof Sequence) || !(tbsFields[0] instanceof Integer)) {
    throw new Error("node CRL: CRL must be an X.509 v2 CRL");
  }
  let version: bigint;
  try {
    version = tbsFields[0].toBigInt();
  } catch {
    throw new Error("node CRL: CRL must be an X.509 v2 CRL");
  }
  if (version !== 1n) throw new Error("node CRL: CRL must be an X.509 v2 CRL");
  if (!tbsFields[1] || !outer[1] || !bytesEqual(rawBlock(tbsFields[1]), rawBlock(outer[1]))) {
    throw new Error("node CRL: inner and outer signature algorithms do not match");
  }
  try {
    const crl = new CertificateRevocationList({ schema: parsed.result });
    if (crl.version !== 1) throw new Error("invalid CRL version");
    return crl;
  } catch {
    throw new Error("node CRL: malformed CRL DER");
  }
}

function extensionSchema(extension: Extension, label: string): ReturnType<typeof fromBER>["result"] {
  return parseDER(
    byteView(extension.extnValue.valueBlock.valueHexView),
    label,
    (schema) => schema,
  );
}

function extensionsByOID(extensions: readonly Extension[] | undefined, oid: string): Extension[] {
  return (extensions ?? []).filter((extension) => extension.extnID === oid);
}

function oneExtension(extensions: readonly Extension[] | undefined, oid: string, label: string): Extension {
  const matches = extensionsByOID(extensions, oid);
  if (matches.length !== 1) throw new Error(`node CRL: issuer ${label} extension count must be 1`);
  return matches[0];
}

function positive20OctetInteger(value: Integer): boolean {
  let number: bigint;
  try {
    number = value.toBigInt();
  } catch {
    return false;
  }
  if (number <= 0n) return false;
  let hex = number.toString(16);
  if (hex.length % 2 !== 0) hex = `0${hex}`;
  const octets = hex.length / 2;
  return octets < 20 || octets === 20 && Number.parseInt(hex.slice(0, 2), 16) < 0x80;
}

function positiveInteger(value: Integer): boolean {
  try {
    return value.toBigInt() > 0n;
  } catch {
    return false;
  }
}

function integerValue(value: number | Integer | undefined): bigint | undefined {
  if (typeof value === "number") return BigInt(value);
  if (value instanceof Integer) return value.toBigInt();
  return undefined;
}

function validateIssuerProfile(
  issuer: Certificate,
  trustDomain: string,
): Uint8Array {
  const basicExtension = oneExtension(issuer.extensions, BASIC_CONSTRAINTS_OID, "basic constraints");
  const basic = new BasicConstraints({ schema: extensionSchema(basicExtension, "issuer basic constraints") });
  if (!basic.cA || integerValue(basic.pathLenConstraint) !== 0n) {
    throw new Error("node CRL: issuer must be a non-delegating intermediate CA");
  }

  const usageExtension = oneExtension(issuer.extensions, KEY_USAGE_OID, "key usage");
  const usage = extensionSchema(usageExtension, "issuer key usage");
  if (!(usage instanceof BitString)
      || usage.valueBlock.unusedBits !== 1
      || !bytesEqual(usage.valueBlock.valueHexView, new Uint8Array([0x06]))) {
    throw new Error("node CRL: issuer has invalid key usage");
  }

  const skidExtension = oneExtension(issuer.extensions, SUBJECT_KEY_ID_OID, "subject key identifier");
  const skid = extensionSchema(skidExtension, "issuer subject key identifier");
  if (!positive20OctetInteger(issuer.serialNumber)
      || !(skid instanceof OctetString)
      || skid.valueBlock.valueHexView.length === 0) {
    throw new Error("node CRL: issuer has invalid identity");
  }

  const policiesExtension = oneExtension(issuer.extensions, CERTIFICATE_POLICIES_OID, "certificate policies");
  const policies = parseCertificatePoliciesDER(byteView(policiesExtension.extnValue.valueBlock.valueHexView));
  if (policies.length !== 1 || !ISSUER_ROLE_POLICIES.has(policies[0])) {
    throw new Error("node CRL: invalid issuer role");
  }

  validateTrustDomain(trustDomain);
  const sanExtension = oneExtension(issuer.extensions, SUBJECT_ALT_NAME_OID, "subject alternative name");
  const san = new AltName({ schema: extensionSchema(sanExtension, "issuer subject alternative name") });
  const uriNames = san.altNames.filter((name) => name.type === 6);
  if (uriNames.length !== 1
      || uriNames[0].value !== `spiffe://${trustDomain}`) {
    throw new Error("node CRL: issuer trust domain does not match configured trust domain");
  }
  return byteView(skid.valueBlock.valueHexView);
}

function validateCRLExtensions(crl: CertificateRevocationList, issuerSKID: Uint8Array): void {
  const extensions = crl.crlExtensions?.extensions ?? [];
  if (extensions.length < 2) throw new Error("node CRL: CRL lacks required extensions");
  if (extensions.length > 2) throw new Error("node CRL: invalid CRL extension count");
  if (extensions.some((extension) => extension.critical)) {
    throw new Error("node CRL: critical CRL extension is unsupported");
  }
  if (extensions.some((extension) => extension.extnID !== AUTHORITY_KEY_ID_OID
    && extension.extnID !== CRL_NUMBER_OID)) {
    throw new Error("node CRL: unsupported CRL extension");
  }

  const akiExtensions = extensionsByOID(extensions, AUTHORITY_KEY_ID_OID);
  const numberExtensions = extensionsByOID(extensions, CRL_NUMBER_OID);
  if (akiExtensions.length !== 1 || numberExtensions.length !== 1) {
    throw new Error("node CRL: CRL lacks required extensions");
  }

  const aki = new AuthorityKeyIdentifier({ schema: extensionSchema(akiExtensions[0], "authority key identifier") });
  if (!aki.keyIdentifier
      || !bytesEqual(byteView(aki.keyIdentifier.valueBlock.valueHexView), issuerSKID)) {
    throw new Error("node CRL: authority key identifier does not match issuer");
  }

  const number = extensionSchema(numberExtensions[0], "CRL number");
  if (!(number instanceof Integer) || !positive20OctetInteger(number)) {
    throw new Error("node CRL: CRL number must be positive and at most 20 DER octets");
  }
}

function validateUpdateWindow(crl: CertificateRevocationList, issuer: Certificate, now: Date): void {
  if (!crl.nextUpdate || !Number.isFinite(crl.thisUpdate.value.getTime())
      || !Number.isFinite(crl.nextUpdate.value.getTime())
      || crl.nextUpdate.value <= crl.thisUpdate.value) {
    throw new Error("node CRL: invalid CRL update window");
  }
  if (crl.thisUpdate.value < issuer.notBefore.value || crl.nextUpdate.value > issuer.notAfter.value) {
    throw new Error("node CRL: CRL update window exceeds issuer validity");
  }
  if (crl.thisUpdate.value > now) throw new Error("node CRL: CRL is not yet valid");
  if (crl.nextUpdate.value <= now) throw new Error("node CRL: CRL is expired");
}

function validateRevocationEntries(crl: CertificateRevocationList): void {
  const serials = new Set<string>();
  for (const entry of crl.revokedCertificates ?? []) {
    if ((entry.crlEntryExtensions?.extensions.length ?? 0) !== 0) {
      throw new Error("node CRL: CRL entry extensions are unsupported");
    }
    if (!positiveInteger(entry.userCertificate)) {
      throw new Error("node CRL: revoked certificate serial must be positive");
    }
    const serial = entry.userCertificate.toBigInt().toString(16);
    if (serials.has(serial)) throw new Error(`node CRL: duplicate revoked certificate serial ${serial}`);
    serials.add(serial);
    if (!Number.isFinite(entry.revocationDate.value.getTime())
        || entry.revocationDate.value > crl.thisUpdate.value) {
      throw new Error(`node CRL: invalid revocation time for serial ${serial}`);
    }
  }
}

/** Verify a node leaf against the immutable issuer/CRL bundle stamped into the SPA. */
export async function verifyNodeCertificateRevocation(
  chainPEM: string,
  rootPEM: string,
  bundles: readonly NodeCRLTrustInput[],
  now: Date,
  trustDomain: string,
): Promise<void> {
  if (!Number.isFinite(now.getTime())) throw new Error("node CRL: verification clock is invalid");

  const chainDER = pemToDerList(chainPEM);
  if (chainDER.length !== 2) {
    throw new Error("node CRL: chain must contain leaf and one signing issuer");
  }
  const rootDER = onePEM(rootPEM, "root certificate");
  const leaf = certificate(chainDER[0], "leaf certificate");
  const chainIssuer = certificate(chainDER[1], "chain issuer certificate");
  const root = certificate(rootDER, "root certificate");
  const cryptoEngine = new RealmSafeCryptoEngine({ name: "spawnery", crypto: globalThis.crypto });

  const matching = bundles.map((bundle) => ({
    bundle,
    issuerDER: onePEM(bundle.issuerPEM, "stamped issuer certificate"),
  })).filter(({ issuerDER }) => bytesEqual(issuerDER, chainDER[1]));
  if (matching.length !== 1) {
    throw new Error("node CRL: chain must match exactly one stamped issuer");
  }

  const issuerRooted = await chainIssuer.verify(root, cryptoEngine);
  if (!issuerRooted) throw new Error("node CRL: stamped issuer is not rooted in the pinned root");
  const issuerSKID = validateIssuerProfile(chainIssuer, trustDomain);

  const crl = revocationList(matching[0].bundle.crlPEM);
  const rawCRLIssuer = crl.issuer.toSchema().valueBeforeDecodeView;
  const rawIssuerSubject = chainIssuer.subject.toSchema().valueBeforeDecodeView;
  if (!bytesEqual(rawCRLIssuer, rawIssuerSubject)) {
    throw new Error("node CRL: CRL issuer does not match stamped issuer subject");
  }
  validateCRLExtensions(crl, issuerSKID);
  validateUpdateWindow(crl, chainIssuer, now);
  validateRevocationEntries(crl);

  let signatureValid = false;
  try {
    signatureValid = await crl.verify({ issuerCertificate: chainIssuer }, cryptoEngine);
  } catch {
    signatureValid = false;
  }
  if (!signatureValid) throw new Error("node CRL: CRL signature does not verify");
  if (crl.isCertificateRevoked(leaf)) throw new Error("node CRL: node certificate is revoked");
}
