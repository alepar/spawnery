export type NodeTrustClass = "cloud" | "self-hosted";

export interface NodeCRLTrustInput {
  class: NodeTrustClass;
  issuerPEM: string;
  crlPEM: string;
}

export interface TrustInputs {
  rootCAPEM: string;
  trustDomain: string;
  cloudAccountId: string;
  nodeCRLs: NodeCRLTrustInput[];
}

export type TrustInputEnvironment = Record<string, string | boolean | undefined>;

const trustInputNames = [
  "VITE_ROOT_CA_PEM",
  "VITE_TRUST_DOMAIN",
  "VITE_CLOUD_ACCOUNT_ID",
  "VITE_NODE_CRLS_JSON",
] as const;

function requiredString(name: string, value: unknown): string {
  if (typeof value !== "string" || !value.trim() || value.toLowerCase().includes("placeholder")) {
    throw new Error(`${name} is required and must contain release trust material`);
  }
  return value.trim();
}

function parseNodeCRLs(raw: string): NodeCRLTrustInput[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error("VITE_NODE_CRLS_JSON must be valid JSON");
  }
  if (!Array.isArray(parsed) || parsed.length !== 2) {
    throw new Error("VITE_NODE_CRLS_JSON must contain exactly cloud and self-hosted entries");
  }

  const classes = new Set<NodeTrustClass>();
  const entries = parsed.map((entry, index): NodeCRLTrustInput => {
    if (typeof entry !== "object" || entry === null || Array.isArray(entry)) {
      throw new Error(`VITE_NODE_CRLS_JSON[${index}] must be an object`);
    }
    const record = entry as Record<string, unknown>;
    const keys = Object.keys(record).sort();
    if (keys.join(",") !== "class,crlPEM,issuerPEM") {
      throw new Error(`VITE_NODE_CRLS_JSON[${index}] must contain exactly class, issuerPEM, and crlPEM`);
    }
    if (record.class !== "cloud" && record.class !== "self-hosted") {
      throw new Error(`VITE_NODE_CRLS_JSON[${index}].class must be cloud or self-hosted`);
    }
    if (classes.has(record.class)) {
      throw new Error(`VITE_NODE_CRLS_JSON has duplicate class ${record.class}`);
    }
    classes.add(record.class);
    return {
      class: record.class,
      issuerPEM: requiredString(`VITE_NODE_CRLS_JSON[${index}].issuerPEM`, record.issuerPEM),
      crlPEM: requiredString(`VITE_NODE_CRLS_JSON[${index}].crlPEM`, record.crlPEM),
    };
  });

  if (!classes.has("cloud") || !classes.has("self-hosted")) {
    throw new Error("VITE_NODE_CRLS_JSON must contain exactly cloud and self-hosted entries");
  }
  return entries;
}

export function validateTrustInputs(
  env: TrustInputEnvironment,
  authRequired: boolean,
): TrustInputs {
  const hasTrustInput = trustInputNames.some((name) => {
    const value = env[name];
    return typeof value === "string" && value.trim() !== "";
  });
  if (!authRequired && !hasTrustInput) {
    return { rootCAPEM: "", trustDomain: "", cloudAccountId: "", nodeCRLs: [] };
  }

  const rootCAPEM = requiredString("VITE_ROOT_CA_PEM", env.VITE_ROOT_CA_PEM);
  const trustDomain = requiredString("VITE_TRUST_DOMAIN", env.VITE_TRUST_DOMAIN);
  const cloudAccountId = requiredString("VITE_CLOUD_ACCOUNT_ID", env.VITE_CLOUD_ACCOUNT_ID);
  const nodeCRLs = parseNodeCRLs(requiredString("VITE_NODE_CRLS_JSON", env.VITE_NODE_CRLS_JSON));
  return { rootCAPEM, trustDomain, cloudAccountId, nodeCRLs };
}
