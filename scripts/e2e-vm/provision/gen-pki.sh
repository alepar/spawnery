#!/usr/bin/env bash
# gen-pki.sh <pkidir> <wildcard-domain> — mint the throwaway PKI for the golden image.
# Uses the baked `spawnery-ca dev` (the tool the code expects) for the spawnery PKI, then adds a
# *.<domain> wildcard TLS cert (for Caddy) signed by that same root CA so a single CA in host trust
# validates everything.
#
# RECONCILE on host: confirm `spawnery-ca dev`'s actual output filenames and map them to the names
# the systemd env (common.env) references (session-key.pem, session-pub.pem, cp-node.crt/key, ca.crt).
set -euo pipefail
PKI="${1:?pkidir}"; DOMAIN="${2:-e2e.test}"
mkdir -p "$PKI"; cd "$PKI"

# 1. spawnery dev PKI (root CA + intermediates + AS Ed25519 session key + CP node-TLS + node identity)
if command -v spawnery-ca >/dev/null; then
  spawnery-ca dev "$PKI"     # writes the dev CA tree here (verify exact filenames on host)
else
  echo "WARN: spawnery-ca not on PATH; falling back to openssl for CA+session (verify shapes)" >&2
  openssl req -x509 -newkey rsa:4096 -nodes -keyout ca.key -out ca.crt -days 3650 \
    -subj "/CN=spawnery-e2e-ca" 2>/dev/null
  openssl genpkey -algorithm ed25519 -out session-key.pem 2>/dev/null
  openssl pkey -in session-key.pem -pubout -out session-pub.pem 2>/dev/null
  # CP node-listener server cert (mTLS :8081)
  openssl req -newkey rsa:2048 -nodes -keyout cp-node.key -out cp-node.csr -subj "/CN=cp-node" 2>/dev/null
  openssl x509 -req -in cp-node.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out cp-node.crt -days 3650 2>/dev/null
fi

# normalize the root CA cert/key filenames this script relies on (spawnery-ca may name them differently)
[ -f ca.crt ] || { for c in root-ca.pem rootCA.crt root.crt; do [ -f "$c" ] && cp -f "$c" ca.crt && break; done; }
[ -f ca.key ] || { for k in root-ca-key.pem rootCA.key root.key; do [ -f "$k" ] && cp -f "$k" ca.key && break; done; }
[ -f ca.crt ] && [ -f ca.key ] || { echo "ERR: root CA cert/key not found in $PKI (RECONCILE spawnery-ca output names)" >&2; exit 1; }

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
openssl x509 -req -in wildcard.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out wildcard.crt -days 3650 -extensions v3 -extfile wildcard.cnf 2>/dev/null
chmod 600 ./*key* 2>/dev/null || true
echo "PKI written to $PKI (ca.crt = host-trust anchor; wildcard.{crt,key} = Caddy TLS for *.$DOMAIN)"
