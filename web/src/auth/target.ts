import type { ResolvedTarget } from "@spawnery/client";
import { verifyCertChain, parseSPIFFEPrincipal, type ParsedCert, type SPIFFEPrincipal } from "@/keys/x509";
import {
  PINNED_ROOT_CA_PEM,
  PINNED_TRUST_DOMAIN,
  PINNED_CLOUD_ACCOUNT_ID,
} from "@/config/trustAnchors";

export interface TargetVerificationPins {
  rootCAPEM: string;
  trustDomain: string;
  cloudAccountId: string;
}

interface TargetVerifierDeps {
  verifyCertChain: (
    chain: string, rootPEM: string, now: Date, trustDomain: string,
  ) => Promise<Pick<ParsedCert, "sanURIs">>;
  parseSPIFFEPrincipal: (raw: string, trustDomain: string) => SPIFFEPrincipal;
}

const defaultPins: TargetVerificationPins = {
  rootCAPEM: PINNED_ROOT_CA_PEM,
  trustDomain: PINNED_TRUST_DOMAIN,
  cloudAccountId: PINNED_CLOUD_ACCOUNT_ID,
};

const defaultDeps: TargetVerifierDeps = { verifyCertChain, parseSPIFFEPrincipal };

/** Verify CP-carried node identity against build-time pins and the typed response fields. */
export async function verifyResolvedTarget(
  target: ResolvedTarget,
  loggedInAccountId: string,
  pins: TargetVerificationPins = defaultPins,
  now: Date = new Date(),
  deps: TargetVerifierDeps = defaultDeps,
): Promise<void> {
  if (!pins.rootCAPEM.trim() || pins.rootCAPEM.includes("PLACEHOLDER") ||
      !pins.trustDomain.trim() || !pins.cloudAccountId.trim() || pins.cloudAccountId.includes("PLACEHOLDER")) {
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
  const leaf = await deps.verifyCertChain(pem, pins.rootCAPEM, now, pins.trustDomain);
  if (leaf.sanURIs.length !== 1) throw new Error("target: node certificate must contain one SPIFFE URI");
  const principal = deps.parseSPIFFEPrincipal(leaf.sanURIs[0], pins.trustDomain);
  if (principal.kind !== "node") throw new Error("target: certificate is not a node identity");
  if (principal.nodeId !== target.targetNodeId || principal.role !== target.targetNodeClass ||
      principal.accountId !== target.targetNodeAccountId) {
    throw new Error("target: typed node identity does not match certificate");
  }
  const expectedAccount = principal.role === "cloud" ? pins.cloudAccountId : loggedInAccountId;
  if (principal.accountId !== expectedAccount) {
    throw new Error("target: node account is not authorized for this placement");
  }
}
