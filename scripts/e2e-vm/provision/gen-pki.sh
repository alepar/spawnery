#!/usr/bin/env bash
# gen-pki.sh <pkidir> <wildcard-domain> — mint the throwaway PKI for the golden image.
# Uses the baked `spawnery-ca dev` (the tool the code expects) for the spawnery PKI, then adds a
# *.<domain> wildcard TLS cert (for Caddy) signed by that same root CA so a single CA in host trust
# validates everything.
#
# `spawnery-ca dev <dir>` writes the root and role intermediates, service SVIDs, certified auth
# signers, current CRLs, and pre-provisioned identities under node/ and node-cloud/.
set -euo pipefail
PKI="${1:?pkidir}"; DOMAIN="${2:-e2e.test}"
mkdir -p "$PKI"; cd "$PKI"

# 1. Spawnery dev PKI (root + intermediates + service SVIDs + certified signers + node identities).
command -v spawnery-ca >/dev/null || {
  echo "ERR: spawnery-ca not on PATH — install the spawnery binaries before running gen-pki.sh" >&2
  exit 1
}
SPAWNERY_TRUST_DOMAIN=prod.spawnery.internal spawnery-ca dev "$PKI"

# host-trust anchor: a copy of the root CA (single CA in host trust validates everything)
cp -f "$PKI/root.pem" "$PKI/ca.crt"

# 2. *.<domain> wildcard cert for Caddy, signed by the root CA
cat > wildcard.cnf <<EOF
[req]
distinguished_name=dn
req_extensions=v3
prompt=no
[dn]
CN=*.$DOMAIN
[v3]
subjectAltName=DNS:*.$DOMAIN,DNS:$DOMAIN
EOF
openssl req -newkey rsa:2048 -nodes -keyout wildcard.key -out wildcard.csr -config wildcard.cnf 2>/dev/null
openssl x509 -req -in wildcard.csr -CA root.pem -CAkey root-key.pem -CAcreateserial \
  -out wildcard.crt -days 3650 -extensions v3 -extfile wildcard.cnf 2>/dev/null

# Runtime bundles contain public trust material plus only the private keys each workload needs.
mkdir -p authsvc cp
cp -f root.pem self-hosted-intermediate.pem self-hosted-intermediate-key.pem \
  service-intermediate.pem cloud-intermediate.pem \
  authsvc-service.pem authsvc-service-chain.pem authsvc-service-key.pem \
  auth-signer-current-key.pem auth-signer-current-chain.pem \
  auth-signer-next-key.pem auth-signer-next-chain.pem \
  service.crl.pem cloud-node.crl.pem self-hosted-node.crl.pem authsvc/
cp -f root.pem service-intermediate.pem cloud-intermediate.pem self-hosted-intermediate.pem \
  cp-service.pem cp-service-chain.pem cp-service-key.pem \
  service.crl.pem cloud-node.crl.pem self-hosted-node.crl.pem cp/
chmod 700 authsvc cp
chmod 600 authsvc/* cp/*

# Root and issuer keys remain ceremony-only. The AS retains only its online self-hosted node issuer
# key in its private runtime bundle; CP and spawnlet never receive issuer keys.
OFFLINE="${SPAWNERY_OFFLINE_PKI_DIR:-$PKI/offline}"
mkdir -p "$OFFLINE"
chmod 700 "$OFFLINE"
mv -f root-key.pem service-intermediate-key.pem cloud-intermediate-key.pem self-hosted-intermediate-key.pem "$OFFLINE/"
mv -f auth-signing-intermediate.pem auth-signing-intermediate-key.pem "$OFFLINE/"
rm -f cp-service-key.pem authsvc-service-key.pem cp-server-key.pem \
  auth-signer-current-key.pem auth-signer-next-key.pem
chmod 600 ./*key* 2>/dev/null || true
echo "PKI written to $PKI (root.pem/ca.crt = host-trust anchor; wildcard.{crt,key} = Caddy TLS for *.$DOMAIN)"
