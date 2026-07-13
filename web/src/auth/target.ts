import type { ResolvedTarget } from "@spawnery/client";
import { verifyCertChain, parseSPIFFEPrincipal, type ParsedCert, type SPIFFEPrincipal } from "@/keys/x509";
import { getTrustAnchors } from "@/config/trustAnchors";
import { verifyNodeCertificateRevocation } from "./crl";
import type { NodeCRLTrustInput } from "../../build/trust-inputs";

export interface TargetVerificationPins {
  rootCAPEM: string;
  trustDomain: string;
  cloudAccountId: string;
  nodeCRLs: NodeCRLTrustInput[];
}

interface TargetVerifierDeps {
  verifyCertChain: (
    chain: string, rootPEM: string, now: Date, trustDomain: string,
  ) => Promise<Pick<ParsedCert, "sanURIs">>;
  parseSPIFFEPrincipal: (raw: string, trustDomain: string) => SPIFFEPrincipal;
  verifyNodeCertificateRevocation: typeof verifyNodeCertificateRevocation;
}

const defaultDeps: TargetVerifierDeps = {
  verifyCertChain,
  parseSPIFFEPrincipal,
  verifyNodeCertificateRevocation,
};

/** Verify CP-carried node identity against build-time pins and the typed response fields. */
export async function verifyResolvedTarget(
  target: ResolvedTarget,
  loggedInAccountId: string,
  pins?: TargetVerificationPins,
  now: Date = new Date(),
  deps: TargetVerifierDeps = defaultDeps,
): Promise<void> {
  const anchors = pins ?? getTrustAnchors();
  if (!anchors.rootCAPEM.trim() || anchors.rootCAPEM.includes("PLACEHOLDER") ||
      !anchors.trustDomain.trim() || !anchors.cloudAccountId.trim() || anchors.cloudAccountId.includes("PLACEHOLDER")) {
    throw new Error("target: release trust pins are not configured");
  }
  if (!loggedInAccountId) throw new Error("target: logged-in account is required");
  if (target.nodeCertChain.length === 0 || !target.targetNodeId ||
      !target.targetNodeClass || !target.targetNodeAccountId) {
    throw new Error("target: incomplete node metadata");
  }
  if (target.targetNodeClass !== "cloud" && target.targetNodeClass !== "self-hosted") {
    throw new Error("target: unsupported node class");
  }

  const pem = new TextDecoder().decode(target.nodeCertChain);
  const leaf = await deps.verifyCertChain(pem, anchors.rootCAPEM, now, anchors.trustDomain);
  if (leaf.sanURIs.length !== 1) throw new Error("target: node certificate must contain one SPIFFE URI");
  const principal = deps.parseSPIFFEPrincipal(leaf.sanURIs[0], anchors.trustDomain);
  if (principal.kind !== "node") throw new Error("target: certificate is not a node identity");
  if (principal.nodeId !== target.targetNodeId || principal.role !== target.targetNodeClass ||
      principal.accountId !== target.targetNodeAccountId) {
    throw new Error("target: typed node identity does not match certificate");
  }
  const expectedAccount = principal.role === "cloud" ? anchors.cloudAccountId : loggedInAccountId;
  if (principal.accountId !== expectedAccount) {
    throw new Error("target: node account is not authorized for this placement");
  }
  await deps.verifyNodeCertificateRevocation(pem, anchors.rootCAPEM, anchors.nodeCRLs, now);
}
