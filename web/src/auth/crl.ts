import { fromBER } from "asn1js";
import { Certificate, CertificateRevocationList, CryptoEngine } from "pkijs";

import type { NodeCRLTrustInput } from "../../build/trust-inputs";
import { pemToDerList } from "@/keys/x509";

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

function byteView(source: BufferSource): Uint8Array<ArrayBuffer> {
  if (ArrayBuffer.isView(source)) {
    return new Uint8Array(source.buffer.slice(
      source.byteOffset,
      source.byteOffset + source.byteLength,
    ) as ArrayBuffer);
  }
  return new Uint8Array(source.slice(0));
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
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
  return parseDER(onePEM(pem, "CRL"), "CRL", (schema) => new CertificateRevocationList({ schema }));
}

/** Verify a node leaf against the immutable issuer/CRL bundle stamped into the SPA. */
export async function verifyNodeCertificateRevocation(
  chainPEM: string,
  rootPEM: string,
  bundles: readonly NodeCRLTrustInput[],
  now: Date,
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

  const crl = revocationList(matching[0].bundle.crlPEM);
  if (!crl.issuer.isEqual(chainIssuer.subject)) {
    throw new Error("node CRL: CRL issuer does not match stamped issuer subject");
  }
  if (crl.thisUpdate.value > now) throw new Error("node CRL: CRL is not current yet");
  if (!crl.nextUpdate) throw new Error("node CRL: nextUpdate is required");
  if (now >= crl.nextUpdate.value) throw new Error("node CRL: CRL has expired");

  let signatureValid = false;
  try {
    signatureValid = await crl.verify({ issuerCertificate: chainIssuer }, cryptoEngine);
  } catch {
    signatureValid = false;
  }
  if (!signatureValid) throw new Error("node CRL: CRL signature does not verify");
  if (crl.isCertificateRevoked(leaf)) throw new Error("node CRL: node certificate is revoked");
}
