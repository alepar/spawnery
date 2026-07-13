import type { DestructiveVMAuthConfig } from "./root-anchored-artifacts";
import { assertDisposableVM, posixShellQuote, ssh } from "./root-anchored-artifacts";

type DestructiveSSH = (cfg: DestructiveVMAuthConfig, command: string) => Promise<string>;

export interface NodeTrustFixtures {
  foreignRootChainPEM: string;
  unstampedIssuerChainPEM: string;
  expiredCRLPEM: string;
  expiredCRLNextUpdateMs: number;
}

export interface ShortLivedNodeCRL {
  crlPEM: string;
  nextUpdateMs: number;
}

function decodePublicPEM(value: string | undefined, field: string): string {
  if (!value || !/^[A-Za-z0-9+/]+={0,2}$/.test(value)) {
    throw new Error(`node trust fixtures: invalid ${field}`);
  }
  const decoded = Buffer.from(value, "base64").toString("utf8");
  if (!decoded.includes("-----BEGIN ") || !decoded.includes("-----END ")) {
    throw new Error(`node trust fixtures: ${field} is not PEM`);
  }
  return decoded;
}

function parseFields(output: string, expected: readonly string[]): Map<string, string> {
  const allowed = new Set(expected);
  const fields = new Map<string, string>();
  for (const line of output.split("\n")) {
    if (!line) continue;
    const separator = line.indexOf("=");
    if (separator <= 0) throw new Error("node trust fixtures: malformed output");
    const name = line.slice(0, separator);
    if (!allowed.has(name)) throw new Error(`node trust fixtures: unexpected field ${name}`);
    if (fields.has(name)) throw new Error(`node trust fixtures: duplicate field ${name}`);
    fields.set(name, line.slice(separator + 1));
  }
  for (const name of expected) {
    if (!fields.has(name)) throw new Error(`node trust fixtures: missing field ${name}`);
  }
  return fields;
}

function parseEpochMillis(value: string | undefined, field: string): number {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error(`node trust fixtures: invalid ${field}`);
  }
  return parsed;
}

export function parseNodeTrustFixtureOutput(output: string): NodeTrustFixtures {
  const fields = parseFields(output, [
    "foreign_root_chain",
    "unstamped_issuer_chain",
    "expired_crl",
    "expired_crl_next_update_ms",
  ]);
  return {
    foreignRootChainPEM: decodePublicPEM(fields.get("foreign_root_chain"), "foreign_root_chain"),
    unstampedIssuerChainPEM: decodePublicPEM(fields.get("unstamped_issuer_chain"), "unstamped_issuer_chain"),
    expiredCRLPEM: decodePublicPEM(fields.get("expired_crl"), "expired_crl"),
    expiredCRLNextUpdateMs: parseEpochMillis(
      fields.get("expired_crl_next_update_ms"),
      "expired_crl_next_update_ms",
    ),
  };
}

function crlConfig(directory: string): string {
  return [
    "[ca]",
    "default_ca=issuer",
    "[issuer]",
    `database=${directory}/index`,
    `new_certs_dir=${directory}`,
    "certificate=/etc/spawnery/node/cloud-intermediate.pem",
    "private_key=/var/lib/spawnery-offline/cloud-intermediate-key.pem",
    "default_md=sha256",
    "default_crl_days=30",
    `crlnumber=${directory}/crlnumber`,
    "crl_extensions=crl_ext",
    "[crl_ext]",
    "authorityKeyIdentifier=keyid:always",
  ].join("\n") + "\n";
}

export function nodeTrustFixtureGenerationCommand(owner: string): string {
  const script = `set -eu
umask 077
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
trust_domain=prod.spawnery.internal
SPAWNERY_TRUST_DOMAIN="$trust_domain" /usr/local/bin/spawnery-ca dev "$d/foreign" >/dev/null 2>&1
cat "$d/foreign/node-cloud/cert.pem" "$d/foreign/node-cloud/chain.pem" | base64 -w0 | sed 's/^/foreign_root_chain=/'
printf '\n'
openssl ecparam -name prime256v1 -genkey -noout -out "$d/issuer.key"
openssl req -new -key "$d/issuer.key" -subj '/CN=Spawnery acceptance unstamped issuer' -out "$d/issuer.csr"
printf '%s\n' '[issuer]' 'basicConstraints=critical,CA:TRUE,pathlen:0' 'keyUsage=critical,keyCertSign,cRLSign' 'subjectKeyIdentifier=hash' 'authorityKeyIdentifier=keyid:always' 'subjectAltName=URI:spiffe://prod.spawnery.internal' 'certificatePolicies=2.25.272377079450377973232136459441396509550' > "$d/issuer.cnf"
openssl x509 -req -in "$d/issuer.csr" -CA /etc/spawnery/node/root.pem -CAkey /var/lib/spawnery-offline/root-key.pem -CAserial "$d/root.srl" -CAcreateserial -days 30 -sha256 -extfile "$d/issuer.cnf" -extensions issuer -out "$d/issuer.pem" >/dev/null 2>&1
openssl ecparam -name prime256v1 -genkey -noout -out "$d/leaf.key"
openssl req -new -key "$d/leaf.key" -subj '/CN=Spawnery acceptance node' -out "$d/leaf.csr"
printf '%s\n' '[leaf]' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature' 'extendedKeyUsage=clientAuth,serverAuth' 'subjectAltName=URI:spiffe://prod.spawnery.internal/node/cloud/spawnery-system/node-1' 'subjectKeyIdentifier=hash' 'authorityKeyIdentifier=keyid:always' > "$d/leaf.cnf"
openssl x509 -req -in "$d/leaf.csr" -CA "$d/issuer.pem" -CAkey "$d/issuer.key" -CAcreateserial -days 1 -sha256 -extfile "$d/leaf.cnf" -extensions leaf -out "$d/leaf.pem" >/dev/null 2>&1
cat "$d/leaf.pem" "$d/issuer.pem" | base64 -w0 | sed 's/^/unstamped_issuer_chain=/'
printf '\n'
mkdir "$d/crl"
touch "$d/crl/index"
printf '02\n' > "$d/crl/crlnumber"
printf %s ${posixShellQuote(crlConfig("."))} > "$d/crl/openssl.cnf"
last=$(date -u -d '15 seconds ago' +%Y%m%d%H%M%SZ)
next_epoch=$(date -u -d '5 seconds ago' +%s)
next=$(date -u -d "@$next_epoch" +%Y%m%d%H%M%SZ)
(cd "$d/crl" && openssl ca -gencrl -batch -config openssl.cnf -crl_lastupdate "$last" -crl_nextupdate "$next" -out expired.pem >/dev/null 2>&1)
openssl crl -in "$d/crl/expired.pem" -noout -verify -CAfile /etc/spawnery/node/cloud-intermediate.pem >/dev/null 2>&1
base64 -w0 "$d/crl/expired.pem" | sed 's/^/expired_crl=/'
printf '\nexpired_crl_next_update_ms=%s000\n' "$next_epoch"`;
  return `sudo sh -c ${posixShellQuote(script)} -- ${posixShellQuote(owner)}`;
}

export async function generateNodeTrustFixtures(
  cfg: DestructiveVMAuthConfig,
  executeSSH: DestructiveSSH = ssh,
): Promise<NodeTrustFixtures> {
  await assertDisposableVM(cfg, executeSSH);
  return parseNodeTrustFixtureOutput(await executeSSH(cfg, nodeTrustFixtureGenerationCommand(cfg.owner)));
}

function shortLivedCRLGenerationCommand(lifetimeSeconds: number): string {
  const script = `set -eu
umask 077
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
touch "$d/index"
printf '03\n' > "$d/crlnumber"
printf %s ${posixShellQuote(crlConfig("."))} > "$d/openssl.cnf"
last=$(date -u -d '5 seconds ago' +%Y%m%d%H%M%SZ)
next_epoch=$(date -u -d '${lifetimeSeconds} seconds' +%s)
next=$(date -u -d "@$next_epoch" +%Y%m%d%H%M%SZ)
(cd "$d" && openssl ca -gencrl -batch -config openssl.cnf -crl_lastupdate "$last" -crl_nextupdate "$next" -out fresh.pem >/dev/null 2>&1)
openssl crl -in "$d/fresh.pem" -noout -verify -CAfile /etc/spawnery/node/cloud-intermediate.pem >/dev/null 2>&1
base64 -w0 "$d/fresh.pem" | sed 's/^/crl=/'
printf '\nnext_update_ms=%s000\n' "$next_epoch"`;
  return `sudo sh -c ${posixShellQuote(script)}`;
}

export async function generateShortLivedNodeCRL(
  cfg: DestructiveVMAuthConfig,
  lifetimeSeconds: number,
  executeSSH: DestructiveSSH = ssh,
): Promise<ShortLivedNodeCRL> {
  if (!Number.isSafeInteger(lifetimeSeconds) || lifetimeSeconds < 10 || lifetimeSeconds > 120) {
    throw new Error("short-lived node CRL lifetime must be an integer from 10 to 120 seconds");
  }
  await assertDisposableVM(cfg, executeSSH);
  const fields = parseFields(await executeSSH(cfg, shortLivedCRLGenerationCommand(lifetimeSeconds)), [
    "crl",
    "next_update_ms",
  ]);
  return {
    crlPEM: decodePublicPEM(fields.get("crl"), "crl"),
    nextUpdateMs: parseEpochMillis(fields.get("next_update_ms"), "next_update_ms"),
  };
}
