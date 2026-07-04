#!/usr/bin/env bash
# gen-pki.sh <pkidir> <wildcard-domain> — mint the throwaway PKI for the golden image.
# Uses the baked `spawnery-ca dev` (the tool the code expects) for the spawnery PKI, then adds a
# *.<domain> wildcard TLS cert (for Caddy) signed by that same root CA so a single CA in host trust
# validates everything.
#
# `spawnery-ca dev <dir>` writes exactly: root.pem, root-key.pem, self-hosted-intermediate.pem(+
# -key.pem), cloud-intermediate.pem(+-key.pem), cp-server.pem(+-key.pem), session-key.pem,
# session-pub.pem, and dirs node/, node-cloud/ — validated against a live reconcile run.
set -euo pipefail
PKI="${1:?pkidir}"; DOMAIN="${2:-e2e.test}"
mkdir -p "$PKI"; cd "$PKI"

# 1. spawnery dev PKI (root CA + intermediates + AS Ed25519 session key + CP node-TLS + node identity)
command -v spawnery-ca >/dev/null || {
  echo "ERR: spawnery-ca not on PATH — install the spawnery binaries before running gen-pki.sh" >&2
  exit 1
}
spawnery-ca dev "$PKI"

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
chmod 600 ./*key* 2>/dev/null || true
echo "PKI written to $PKI (root.pem/ca.crt = host-trust anchor; wildcard.{crt,key} = Caddy TLS for *.$DOMAIN)"
